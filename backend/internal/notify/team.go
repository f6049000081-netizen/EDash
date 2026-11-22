package notify

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"superapp/backend/internal/outbox"
)

type teamEvent struct {
	MerchantID   uuid.UUID  `json:"merchant_id"`
	InvitationID uuid.UUID  `json:"invitation_id"`
	UserID       *uuid.UUID `json:"user_id"` // the person invited, joining, changed or removed; null for an invitation to a number not on EDash
	By           uuid.UUID  `json:"by"`
	Role         string     `json:"role"`
}

// team tells people about invitations and changes to a shop's team: the person concerned, or the
// shop's owners when someone joins, declines or leaves.
func (s *Service) team(ctx context.Context, m outbox.Message, ev teamEvent) error {
	if ev.UserID == nil {
		return nil // invited by number before they joined EDash: they'll see it in the app after signing up
	}
	var shop string
	if err := s.Pool.QueryRow(ctx, `SELECT trade_name FROM merchant.merchants WHERE id = $1`, ev.MerchantID).Scan(&shop); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	args := map[string]string{"shop": shop, "role": ev.Role}
	data := map[string]string{"kind": "team", "merchant_id": ev.MerchantID.String()}
	switch m.Topic {
	case "team.invited":
		name, err := s.personName(ctx, ev.By)
		if err != nil {
			return err
		}
		args["name"] = name
		data["invitation_id"] = ev.InvitationID.String()
		return s.add(ctx, m, *ev.UserID, "team_invited", args, data)
	case "team.role_changed":
		return s.add(ctx, m, *ev.UserID, "team_role_changed", args, data)
	case "team.removed":
		return s.add(ctx, m, *ev.UserID, "team_removed", args, data)
	case "team.joined", "team.declined", "team.left":
		name, err := s.personName(ctx, *ev.UserID)
		if err != nil {
			return err
		}
		args["name"] = name
		rows, err := s.Pool.Query(ctx, `SELECT user_id FROM merchant.members WHERE merchant_id = $1 AND role = 'owner'`, ev.MerchantID)
		if err != nil {
			return err
		}
		owners, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		kind := map[string]string{"team.joined": "team_joined", "team.declined": "team_declined", "team.left": "team_left"}[m.Topic]
		for _, o := range owners {
			if err := s.add(ctx, m, o, kind, args, data); err != nil {
				return err
			}
		}
	}
	return nil
}
