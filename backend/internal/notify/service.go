// Package notify turns outbox events into notifications in the user's language, keeps them for the
// in-app list, and pushes them to the user's devices when a push provider is configured.
package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/money"
	"superapp/backend/internal/outbox"
)

// Topics this service consumes.
var Topics = []string{
	"payment.succeeded", "payment.failed", "payment.held",
	"identity.signed_in", "identity.pin_reset", "identity.pin_changed", "identity.refresh_token_reused",
	"identity.biometric_enrolled",
	"payout.settled", "payout.on_hold", "merchant.payout_destination_changed", "merchant.applied", "merchant.approved",
}

// Push is what a device shows. Data lets the app open the right screen.
type Push struct {
	Title, Body string
	Data        map[string]string
}

// Pusher sends to one device token. ErrTokenInvalid means the token is gone for good (app uninstalled,
// token rotated) and should be forgotten.
type Pusher interface {
	Push(ctx context.Context, token string, p Push) error
}

var ErrTokenInvalid = errors.New("notify: push token no longer valid")

type Service struct {
	Pool   *pgxpool.Pool
	Pusher Pusher // nil: notifications are kept for the in-app list only
	Log    *slog.Logger
}

var railNames = map[string]string{
	"telebirr": "telebirr", "cbe_birr": "CBE Birr", "mpesa_et": "M-PESA", "ethswitch": "your bank",
	"card": "your card", "remittance": "abroad", "sandbox": "test money (sandbox)",
}

type paymentEvent struct {
	IntentID    uuid.UUID   `json:"intent_id"`
	Kind        string      `json:"kind"`
	PayerID     uuid.UUID   `json:"payer_user_id"`
	PayeeType   string      `json:"payee_type"`
	PayeeID     uuid.UUID   `json:"payee_id"`
	Amount      money.Money `json:"amount"`
	Status      string      `json:"status"`
	FailureCode string      `json:"failure_code"`
	Rail        string      `json:"rail"`
}

type identityEvent struct {
	UserID    uuid.UUID `json:"user_id"`
	DeviceID  uuid.UUID `json:"device_id"`
	NewDevice bool      `json:"new_device"`
	Platform  string    `json:"platform"`
	Purpose   string    `json:"purpose"`
}

// Handle implements outbox.Handler. It is idempotent: a redelivered event adds nothing.
func (s *Service) Handle(ctx context.Context, m outbox.Message) error {
	switch {
	case strings.HasPrefix(m.Topic, "payout.") || strings.HasPrefix(m.Topic, "merchant."):
		var ev merchantEvent
		if err := json.Unmarshal(m.Payload, &ev); err != nil {
			s.warn("unreadable merchant event", m, err)
			return nil
		}
		return s.merchant(ctx, m, ev)
	case strings.HasPrefix(m.Topic, "payment."):
		var ev paymentEvent
		if err := json.Unmarshal(m.Payload, &ev); err != nil {
			s.warn("unreadable payment event", m, err)
			return nil // a malformed event will never parse; retrying would block every later event
		}
		return s.payment(ctx, m, ev)
	case strings.HasPrefix(m.Topic, "identity."):
		var ev identityEvent
		if err := json.Unmarshal(m.Payload, &ev); err != nil {
			s.warn("unreadable identity event", m, err)
			return nil
		}
		return s.security(ctx, m, ev)
	}
	return nil
}

func (s *Service) payment(ctx context.Context, m outbox.Message, ev paymentEvent) error {
	amount := ev.Amount.Display()
	data := map[string]string{"intent_id": ev.IntentID.String()}

	if ev.Kind == "p2p" && ev.Status == "succeeded" && ev.PayeeType == "user" {
		payer, err := s.personName(ctx, ev.PayerID)
		if err != nil {
			return err
		}
		if err := s.add(ctx, m, ev.PayeeID, "money_received", map[string]string{"name": payer, "amount": amount}, data); err != nil {
			return err
		}
	}

	// The payer saw the result on screen when it was decided while they confirmed. Tell them only about
	// outcomes that came later: top-ups (decided by the partner) and payments that were held or processing.
	async, err := s.decidedLater(ctx, ev.IntentID, ev.Status)
	if err != nil || !async {
		return err
	}
	args := map[string]string{"amount": amount, "rail": railNames[ev.Rail]}
	var kind string
	if ev.Kind == "topup" {
		kind = map[string]string{"succeeded": "topup_succeeded", "failed": "topup_failed", "held_for_review": "topup_held"}[ev.Status]
	} else {
		kind = map[string]string{"succeeded": "payment_succeeded", "failed": "payment_failed"}[ev.Status]
		if args["name"], err = s.payeeName(ctx, ev.PayeeType, ev.PayeeID); err != nil {
			return err
		}
	}
	if kind == "" {
		return nil
	}
	return s.add(ctx, m, ev.PayerID, kind, args, data)
}

// decidedLater reports whether the transition into status started from processing or held_for_review.
func (s *Service) decidedLater(ctx context.Context, intent uuid.UUID, status string) (bool, error) {
	var from *string
	err := s.Pool.QueryRow(ctx, `
		SELECT from_status::text FROM payments.intent_events
		WHERE intent_id = $1 AND to_status = $2::payments.intent_status ORDER BY id DESC LIMIT 1`,
		intent, status).Scan(&from)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return from != nil && (*from == "processing" || *from == "held_for_review"), nil
}

func (s *Service) security(ctx context.Context, m outbox.Message, ev identityEvent) error {
	var kind string
	args := map[string]string{}
	switch m.Topic {
	case "identity.signed_in":
		if !ev.NewDevice {
			return nil
		}
		// Devices the account had before this one. Counted against this device's first sighting, not
		// now: the event may be handled after more devices were added.
		var earlier int
		if err := s.Pool.QueryRow(ctx, `
			SELECT count(*) FROM identity.devices d, identity.devices this
			WHERE this.id = $2 AND d.user_id = $1 AND d.id <> this.id AND d.first_seen_at <= this.first_seen_at`,
			ev.UserID, ev.DeviceID).Scan(&earlier); err != nil {
			return err
		}
		if earlier == 0 {
			return nil // the account's first device: that's signing up, not a warning
		}
		kind = "new_device"
		args["platform"] = map[string]string{"android": "Android", "ios": "iPhone", "web": "web"}[ev.Platform]
	case "identity.pin_reset":
		kind = "pin_reset"
	case "identity.pin_changed":
		kind = "pin_changed"
	case "identity.refresh_token_reused":
		kind = "session_revoked"
	case "identity.biometric_enrolled":
		kind = "biometric_enrolled"
	default:
		return nil
	}
	return s.add(ctx, m, ev.UserID, kind, args, map[string]string{"kind": "security"})
}

type merchantEvent struct {
	MerchantID  uuid.UUID   `json:"merchant_id"`
	PayoutID    uuid.UUID   `json:"payout_id"`
	Net         money.Money `json:"net"`
	Reason      string      `json:"reason"`
	Destination string      `json:"destination"`
	Label       string      `json:"label"`
	EffectiveAt time.Time   `json:"effective_at"`
}

// merchant tells a shop's owners and accountants about payouts and payout-account changes.
func (s *Service) merchant(ctx context.Context, m outbox.Message, ev merchantEvent) error {
	var shop string
	if err := s.Pool.QueryRow(ctx, `SELECT trade_name FROM merchant.merchants WHERE id = $1`, ev.MerchantID).Scan(&shop); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	args := map[string]string{"shop": shop, "amount": ev.Net.Display(), "destination": ev.Destination}
	data := map[string]string{"merchant_id": ev.MerchantID.String()}
	var kind string
	switch m.Topic {
	case "payout.settled":
		kind = "payout_settled"
		data["payout_id"] = ev.PayoutID.String()
	case "payout.on_hold":
		kind = "payout_on_hold_" + ev.Reason
		if _, ok := texts["en"][kind+".body"]; !ok {
			kind = "payout_on_hold"
		}
		data["payout_id"] = ev.PayoutID.String()
	case "merchant.applied":
		kind = "merchant_applied"
	case "merchant.approved":
		kind = "merchant_approved"
	case "merchant.payout_destination_changed":
		kind = "payout_destination_changed"
		args["destination"] = ev.Label
		args["when"] = ev.EffectiveAt.In(time.FixedZone("EAT", 3*60*60)).Format("2 Jan 2006, 15:04")
		data["kind"] = "security"
	default:
		return nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT user_id FROM merchant.members WHERE merchant_id = $1 AND role IN ('owner', 'accountant')`, ev.MerchantID)
	if err != nil {
		return err
	}
	users, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, u := range users {
		if err := s.add(ctx, m, u, kind, args, data); err != nil {
			return err
		}
	}
	return nil
}

// add stores one notification and pushes it. A repeat of the same event for the same user is ignored.
func (s *Service) add(ctx context.Context, m outbox.Message, user uuid.UUID, kind string, args, data map[string]string) error {
	var locale string
	err := s.Pool.QueryRow(ctx, `SELECT locale::text FROM identity.users WHERE id = $1`, user).Scan(&locale)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	title, body := text(locale, kind+".title", args), text(locale, kind+".body", args)
	var id uuid.UUID
	err = s.Pool.QueryRow(ctx, `
		INSERT INTO notify.notifications (user_id, kind, title, body, data, source_event)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id, source_event, kind) DO NOTHING
		RETURNING id`, user, kind, title, body, data, m.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already delivered
	}
	if err != nil {
		return err
	}
	if s.Pusher != nil {
		pd := map[string]string{"notification_id": id.String()}
		for k, v := range data {
			pd[k] = v
		}
		s.push(ctx, user, id, Push{Title: title, Body: body, Data: pd})
	}
	return nil
}

// push is best effort: the notification is already stored and shows in the app either way. It goes only
// to devices that still have a live session, so a device signed out by a PIN reset or a token-reuse
// revocation stops receiving the user's notifications even if its token is still on file.
func (s *Service) push(ctx context.Context, user, id uuid.UUID, p Push) {
	rows, err := s.Pool.Query(ctx, `
		SELECT t.device_id, t.token FROM notify.push_tokens t
		WHERE t.user_id = $1 AND EXISTS (
			SELECT 1 FROM identity.sessions se
			WHERE se.device_id = t.device_id AND se.user_id = $1 AND se.revoked_at IS NULL AND se.expires_at > now())`, user)
	if err != nil {
		s.warn("push tokens", outbox.Message{}, err)
		return
	}
	type tok struct {
		Device uuid.UUID
		Token  string
	}
	toks, err := pgx.CollectRows(rows, pgx.RowToStructByPos[tok])
	if err != nil {
		s.warn("push tokens", outbox.Message{}, err)
		return
	}
	sent := false
	for _, t := range toks {
		err := s.Pusher.Push(ctx, t.Token, p)
		switch {
		case err == nil:
			sent = true
		case errors.Is(err, ErrTokenInvalid):
			_, _ = s.Pool.Exec(ctx, `DELETE FROM notify.push_tokens WHERE device_id = $1 AND token = $2`, t.Device, t.Token)
		default:
			if s.Log != nil {
				s.Log.Warn("push failed", "notification", id, "err", err)
			}
		}
	}
	if sent {
		_, _ = s.Pool.Exec(ctx, `UPDATE notify.notifications SET pushed_at = now() WHERE id = $1`, id)
	}
}

func (s *Service) personName(ctx context.Context, user uuid.UUID) (string, error) {
	var name, phone string
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE(display_name, ''), phone_e164 FROM identity.users WHERE id = $1`, user).Scan(&name, &phone)
	if err != nil {
		return "", err
	}
	if name != "" {
		return name, nil
	}
	return "•••• " + phone[max(0, len(phone)-4):], nil
}

func (s *Service) payeeName(ctx context.Context, payeeType string, id uuid.UUID) (string, error) {
	if payeeType == "user" {
		return s.personName(ctx, id)
	}
	var name string
	err := s.Pool.QueryRow(ctx, `SELECT trade_name FROM merchant.merchants WHERE id = $1`, id).Scan(&name)
	return name, err
}

func (s *Service) warn(msg string, m outbox.Message, err error) {
	if s.Log != nil {
		s.Log.Warn(msg, "topic", m.Topic, "outbox_id", m.ID, "err", err)
	}
}

// ---------------------------------------------------------------------------
// The user's list
// ---------------------------------------------------------------------------

type Notification struct {
	ID        uuid.UUID
	Kind      string
	Title     string
	Body      string
	Data      map[string]string
	CreatedAt time.Time
	Read      bool
}

type Page struct {
	Items      []Notification
	NextCursor string
	Unread     int
}

// ErrBadCursor means the cursor wasn't one this service issued.
var ErrBadCursor = errors.New("notify: bad cursor")

// List returns the user's notifications, newest first, and how many are unread.
func (s *Service) List(ctx context.Context, user uuid.UUID, cursor string, limit int) (*Page, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	before, beforeID := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), uuid.Max
	if cursor != "" {
		var err error
		if before, beforeID, err = decodeCursor(cursor); err != nil {
			return nil, ErrBadCursor
		}
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, kind, title, body, data, created_at, read_at IS NOT NULL FROM notify.notifications
		WHERE user_id = $1 AND (created_at, id) < ($2, $3)
		ORDER BY created_at DESC, id DESC LIMIT $4`, user, before, beforeID, limit+1)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Notification, error) {
		var n Notification
		err := r.Scan(&n.ID, &n.Kind, &n.Title, &n.Body, &n.Data, &n.CreatedAt, &n.Read)
		return n, err
	})
	if err != nil {
		return nil, err
	}
	p := &Page{Items: items}
	if len(items) > limit {
		p.Items = items[:limit]
		last := p.Items[limit-1]
		p.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	if p.Items == nil {
		p.Items = []Notification{}
	}
	err = s.Pool.QueryRow(ctx, `SELECT count(*) FROM notify.notifications WHERE user_id = $1 AND read_at IS NULL`, user).Scan(&p.Unread)
	return p, err
}

// MarkRead marks the given notifications read, or all of them when ids is empty. Other users' ids are
// ignored. It returns how many changed.
func (s *Service) MarkRead(ctx context.Context, user uuid.UUID, ids []uuid.UUID) (int64, error) {
	var tag interface{ RowsAffected() int64 }
	var err error
	if len(ids) == 0 {
		tag, err = s.Pool.Exec(ctx, `UPDATE notify.notifications SET read_at = now() WHERE user_id = $1 AND read_at IS NULL`, user)
	} else {
		tag, err = s.Pool.Exec(ctx, `UPDATE notify.notifications SET read_at = now() WHERE user_id = $1 AND id = ANY($2) AND read_at IS NULL`, user, ids)
	}
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ValidationError is a problem with what the client sent.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// RegisterPushToken stores the push token for the signed-in device, replacing any earlier one. A token
// moves with the device: if another account had it (sign-out, then a different sign-in), it's taken over.
func (s *Service) RegisterPushToken(ctx context.Context, user, device uuid.UUID, token string) error {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 4096 {
		return &ValidationError{"Send the push token the device received from Firebase."}
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var platform string
		err := tx.QueryRow(ctx, `SELECT platform FROM identity.devices WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, device, user).Scan(&platform)
		if errors.Is(err, pgx.ErrNoRows) {
			return &ValidationError{"This device isn't signed in."}
		}
		if err != nil {
			return err
		}
		if platform != "android" && platform != "ios" {
			return &ValidationError{"Push notifications are available on Android and iPhone."}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM notify.push_tokens WHERE token = $1 AND device_id <> $2`, token, device); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO notify.push_tokens (device_id, user_id, token, platform) VALUES ($1, $2, $3, $4)
			ON CONFLICT (device_id) DO UPDATE SET user_id = $2, token = $3, platform = $4, updated_at = now()`,
			device, user, token, platform)
		return err
	})
}

// RemovePushToken forgets the device's token (sign-out, or the user turned notifications off).
func (s *Service) RemovePushToken(ctx context.Context, device uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM notify.push_tokens WHERE device_id = $1`, device)
	return err
}

func encodeCursor(t time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.UnixNano(), 10) + "." + id.String()))
}

func decodeCursor(c string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	ns, idStr, ok := strings.Cut(string(raw), ".")
	if !ok {
		return time.Time{}, uuid.Nil, fmt.Errorf("bad cursor")
	}
	n, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	id, err := uuid.Parse(idStr)
	return time.Unix(0, n).UTC(), id, err
}
