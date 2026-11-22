package merchants

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"superapp/backend/internal/platform"
)

// A shop's team. Owners invite people by phone number as managers, cashiers or accountants; the person
// accepts in the app, so nobody joins a business without agreeing. Owners change roles and remove people,
// and anyone but an owner can leave. Owners themselves aren't added, changed or removed here: who owns a
// business is part of its KYB record and changes through operations.

const (
	// InviteFor is how long an invitation can be accepted.
	InviteFor = 7 * 24 * time.Hour
	// MaxTeam caps a shop's members, and MaxPending its open invitations.
	MaxTeam    = 50
	MaxPending = 20
)

// TeamRoles are the roles an owner can give.
var TeamRoles = []string{"manager", "cashier", "accountant"}

func teamRole(r string) bool {
	for _, x := range TeamRoles {
		if r == x {
			return true
		}
	}
	return false
}

var (
	ErrInvitationGone = errors.New("merchants: invitation expired, answered or cancelled")
	phoneE164RE       = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
)

// NormalizePhone accepts "+251 91 123 4567", "0911234567" or "911234567" (Ethiopian numbers) and any
// other number in international form. It returns "" when it can't tell.
func NormalizePhone(p string) string {
	p = strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '(' || r == ')' || r == '.' {
			return -1
		}
		return r
	}, strings.TrimSpace(p))
	switch {
	case strings.HasPrefix(p, "00"):
		p = "+" + p[2:]
	case len(p) == 10 && (strings.HasPrefix(p, "09") || strings.HasPrefix(p, "07")):
		p = "+251" + p[1:]
	case len(p) == 9 && (p[0] == '9' || p[0] == '7'):
		p = "+251" + p
	case strings.HasPrefix(p, "251"):
		p = "+" + p
	}
	if !phoneE164RE.MatchString(p) {
		return ""
	}
	return p
}

// MaskPhone shows only the last four digits.
func MaskPhone(p string) string { return "••" + p[max(0, len(p)-4):] }

type Member struct {
	UserID uuid.UUID
	Name   string
	Phone  string // masked
	Role   string
	Since  time.Time
}

type Invitation struct {
	ID         uuid.UUID
	MerchantID uuid.UUID
	Shop       string
	Phone      string // full for the shop's owners; the invitee knows their own number
	Role       string
	InvitedBy  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

type Team struct {
	Members     []Member
	Invitations []Invitation // owners only
}

// lockShop takes the shop's row lock, so team changes for one shop happen one at a time, and returns
// the caller's role. Shops that are closed, rejected or suspended can't change their team.
func (s *Service) lockShop(ctx context.Context, tx pgx.Tx, user, merchant uuid.UUID) (string, error) {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM merchant.merchants WHERE id = $1 FOR UPDATE`, merchant).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	role, err := s.role(ctx, tx, user, merchant)
	if err != nil {
		return "", err
	}
	if status != "active" && status != "applied" && status != "kyb_review" {
		return "", &ConflictError{"The team can't be changed while the shop isn't active."}
	}
	return role, nil
}

// Team lists the shop's members, owners first. Owners and managers see it; owners also see open invitations.
func (s *Service) Team(ctx context.Context, user, merchant uuid.UUID) (*Team, error) {
	role, err := s.role(ctx, s.Pool, user, merchant)
	if err != nil {
		return nil, err
	}
	if role != "owner" && role != "manager" {
		return nil, ErrForbidden
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT mm.user_id, COALESCE(u.display_name, ''), u.phone_e164, mm.role, mm.created_at
		FROM merchant.members mm JOIN identity.users u ON u.id = mm.user_id
		WHERE mm.merchant_id = $1
		ORDER BY array_position(ARRAY['owner', 'manager', 'accountant', 'cashier'], mm.role), mm.created_at, mm.user_id`, merchant)
	if err != nil {
		return nil, err
	}
	t := &Team{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Name, &m.Phone, &m.Role, &m.Since); err != nil {
			rows.Close()
			return nil, err
		}
		m.Phone = MaskPhone(m.Phone)
		t.Members = append(t.Members, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if role == "owner" {
		if t.Invitations, err = s.invitations(ctx, `i.merchant_id = $1`, merchant); err != nil {
			return nil, err
		}
	}
	return t, nil
}

func (s *Service) invitations(ctx context.Context, where string, arg any) ([]Invitation, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT i.id, i.merchant_id, m.trade_name, i.phone_e164, i.role, COALESCE(u.display_name, ''), i.created_at, i.expires_at
		FROM merchant.invitations i
		JOIN merchant.merchants m ON m.id = i.merchant_id
		JOIN identity.users u ON u.id = i.invited_by
		WHERE `+where+` AND i.status = 'pending' AND i.expires_at > $2
		ORDER BY i.created_at DESC, i.id`, arg, s.now())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Invitation])
}

// Invite asks the person with this phone number to join the shop's team. Inviting the same number again
// replaces the earlier invitation. The person is told in the app if they have an account; otherwise the
// invitation is there when they sign up with that number.
func (s *Service) Invite(ctx context.Context, user, merchant uuid.UUID, phone, role string) (*Invitation, error) {
	phone = NormalizePhone(phone)
	switch {
	case phone == "":
		return nil, &ValidationError{"Write the phone number with its country code, for example +251 91 123 4567."}
	case role == "owner":
		return nil, &ValidationError{"Owners can't be invited. Ask operations to add an owner to the business record."}
	case !teamRole(role):
		return nil, &ValidationError{"Choose manager, cashier or accountant."}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	now := s.now()
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		myRole, err := s.lockShop(ctx, tx, user, merchant)
		if err != nil {
			return err
		}
		if myRole != "owner" {
			return ErrForbidden
		}
		var invitee *uuid.UUID
		var member bool
		if err := tx.QueryRow(ctx, `
			SELECT u.id, EXISTS (SELECT 1 FROM merchant.members mm WHERE mm.merchant_id = $2 AND mm.user_id = u.id)
			FROM identity.users u WHERE u.phone_e164 = $1`, phone, merchant).Scan(&invitee, &member); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if member {
			return &ConflictError{"That number is already on the team. Change their role instead."}
		}
		if _, err := tx.Exec(ctx, `UPDATE merchant.invitations SET status = 'cancelled', answered_at = $3, answered_by = $4
			WHERE merchant_id = $1 AND phone_e164 = $2 AND status = 'pending'`, merchant, phone, now, user); err != nil {
			return err
		}
		var members, pending int
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM merchant.members WHERE merchant_id = $1),
			(SELECT count(*) FROM merchant.invitations WHERE merchant_id = $1 AND status = 'pending' AND expires_at > $2)`,
			merchant, now).Scan(&members, &pending); err != nil {
			return err
		}
		switch {
		case members+pending >= MaxTeam:
			return &ConflictError{"A shop's team can have up to 50 people, counting open invitations."}
		case pending >= MaxPending:
			return &ConflictError{"There are already 20 open invitations. Cancel some first."}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO merchant.invitations (id, merchant_id, phone_e164, role, invited_by, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, merchant, phone, role, user, now, now.Add(InviteFor)); err != nil {
			return err
		}
		return platform.WriteOutbox(ctx, tx, "team.invited", merchant, map[string]any{
			"merchant_id": merchant, "invitation_id": id, "user_id": invitee, "by": user, "role": role,
		})
	})
	if err != nil {
		return nil, err
	}
	list, err := s.invitations(ctx, `i.id = $1`, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return &list[0], nil
}

// CancelInvitation withdraws an open invitation (owners).
func (s *Service) CancelInvitation(ctx context.Context, user, merchant, invitation uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		role, err := s.lockShop(ctx, tx, user, merchant)
		if err != nil {
			return err
		}
		if role != "owner" {
			return ErrForbidden
		}
		tag, err := tx.Exec(ctx, `UPDATE merchant.invitations SET status = 'cancelled', answered_at = $3, answered_by = $4
			WHERE id = $1 AND merchant_id = $2 AND status = 'pending'`, invitation, merchant, s.now(), user)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrInvitationGone
		}
		return nil
	})
}

// MyInvitations lists the open invitations for the caller's phone number.
func (s *Service) MyInvitations(ctx context.Context, user uuid.UUID) ([]Invitation, error) {
	return s.invitations(ctx, `i.phone_e164 = (SELECT phone_e164 FROM identity.users WHERE id = $1)
		AND m.status IN ('active', 'applied', 'kyb_review')`, user)
}

// AnswerInvitation accepts or declines an invitation sent to the caller's phone number. Accepting adds
// them to the team with the role offered.
func (s *Service) AnswerInvitation(ctx context.Context, user, invitation uuid.UUID, accept bool) (*Merchant, error) {
	var merchant uuid.UUID
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var phone, role, status, myPhone, userStatus string
		var expires time.Time
		err := tx.QueryRow(ctx, `SELECT merchant_id, phone_e164, role, status, expires_at FROM merchant.invitations WHERE id = $1`,
			invitation).Scan(&merchant, &phone, &role, &status, &expires)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT phone_e164, status FROM identity.users WHERE id = $1`, user).Scan(&myPhone, &userStatus); err != nil {
			return err
		}
		if myPhone != phone {
			return ErrNotFound // someone else's invitation
		}
		// The shop lock first, then the invitation again: the same order Invite and CancelInvitation use.
		var shopStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM merchant.merchants WHERE id = $1 FOR UPDATE`, merchant).Scan(&shopStatus); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT status FROM merchant.invitations WHERE id = $1 FOR UPDATE`, invitation).Scan(&status); err != nil {
			return err
		}
		now := s.now()
		if status != "pending" || !expires.After(now) {
			return ErrInvitationGone
		}
		if accept && shopStatus != "active" && shopStatus != "applied" && shopStatus != "kyb_review" {
			return &ConflictError{"This shop can't add people right now."}
		}
		if accept && userStatus != "active" {
			return &ConflictError{"Set up your PIN before joining a shop."}
		}
		answer := "declined"
		if accept {
			answer = "accepted"
		}
		if _, err := tx.Exec(ctx, `UPDATE merchant.invitations SET status = $2, answered_at = $3, answered_by = $4 WHERE id = $1`,
			invitation, answer, now, user); err != nil {
			return err
		}
		topic := "team.declined"
		if accept {
			topic = "team.joined"
			tag, err := tx.Exec(ctx, `INSERT INTO merchant.members (merchant_id, user_id, role, created_at) VALUES ($1, $2, $3, $4)
				ON CONFLICT (merchant_id, user_id) DO NOTHING`, merchant, user, role, now)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return nil // already on the team, with whatever role they have: nothing changes
			}
		}
		return platform.WriteOutbox(ctx, tx, topic, merchant, map[string]any{
			"merchant_id": merchant, "invitation_id": invitation, "user_id": user, "role": role,
		})
	})
	if err != nil {
		return nil, err
	}
	if !accept {
		return nil, nil
	}
	return s.Get(ctx, user, merchant)
}

// SetRole changes a member's role (owners). Owners' own roles don't change here.
func (s *Service) SetRole(ctx context.Context, user, merchant, member uuid.UUID, role string) error {
	if !teamRole(role) {
		return &ValidationError{"Choose manager, cashier or accountant."}
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		myRole, err := s.lockShop(ctx, tx, user, merchant)
		if err != nil {
			return err
		}
		if myRole != "owner" {
			return ErrForbidden
		}
		current, err := s.role(ctx, tx, member, merchant)
		if err != nil {
			return err
		}
		if current == "owner" {
			return &ConflictError{"Owners' roles can't be changed here. Ask operations to change the business record."}
		}
		if current == role {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE merchant.members SET role = $3 WHERE merchant_id = $1 AND user_id = $2`,
			merchant, member, role); err != nil {
			return err
		}
		return platform.WriteOutbox(ctx, tx, "team.role_changed", merchant, map[string]any{
			"merchant_id": merchant, "user_id": member, "by": user, "role": role, "from": current,
		})
	})
}

// RemoveMember takes someone off the team: an owner removing a manager, cashier or accountant, or a
// non-owner leaving. Their computer sign-ins for the shop end at once.
func (s *Service) RemoveMember(ctx context.Context, user, merchant, member uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM merchant.merchants WHERE id = $1 FOR UPDATE`, merchant).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		myRole, err := s.role(ctx, tx, user, merchant)
		if err != nil {
			return err
		}
		current, err := s.role(ctx, tx, member, merchant)
		if err != nil {
			return err
		}
		leaving := member == user
		switch {
		case current == "owner" && leaving:
			return &ConflictError{"An owner can't leave the business here. Ask operations to change the business record."}
		case current == "owner":
			return &ConflictError{"Owners can't be removed here. Ask operations to change the business record."}
		case !leaving && myRole != "owner":
			return ErrForbidden
		}
		now := s.now()
		if _, err := tx.Exec(ctx, `DELETE FROM merchant.members WHERE merchant_id = $1 AND user_id = $2`, merchant, member); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE merchant.web_sessions SET revoked_at = $3
			WHERE merchant_id = $1 AND user_id = $2 AND revoked_at IS NULL`, merchant, member, now); err != nil {
			return err
		}
		topic := "team.removed"
		if leaving {
			topic = "team.left"
		}
		return platform.WriteOutbox(ctx, tx, topic, merchant, map[string]any{
			"merchant_id": merchant, "user_id": member, "by": user, "role": current,
		})
	})
}
