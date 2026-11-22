package kyb_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/kyb"
	"superapp/backend/internal/kyb/kybtest"
)

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
)

func keySpec(id string) string {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return id + ":" + base64.StdEncoding.EncodeToString(k)
}

type env struct {
	t     *testing.T
	ctx   context.Context
	svc   *kyb.Service
	store *kybtest.MemStore
}

func setup(t *testing.T) *env {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	poolOnce.Do(func() { pool, poolErr = pgxpool.New(context.Background(), dsn) })
	if poolErr != nil {
		t.Fatal(poolErr)
	}
	sealer, err := kyb.ParseKeys(keySpec("k1"))
	if err != nil {
		t.Fatal(err)
	}
	store := kybtest.NewMemStore()
	return &env{t: t, ctx: context.Background(), store: store, svc: &kyb.Service{Pool: pool, Store: store, Sealer: sealer}}
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) user() uuid.UUID {
	id := uuid.New()
	e.exec(`INSERT INTO identity.users (id, phone_e164, status) VALUES ($1, $2, 'active')`,
		id, fmt.Sprintf("+2519%08d", binary.BigEndian.Uint64(id[:8])%100_000_000))
	return id
}

// shop is an application under review, owned by owner; licence says whether it gave a trade licence number.
func (e *env) shop(owner uuid.UUID, licence bool) uuid.UUID {
	id := uuid.New()
	var lic *string
	if licence {
		s := "AA/14/" + id.String()[:6]
		lic = &s
	}
	e.exec(`INSERT INTO merchant.merchants (id, legal_name, trade_name, category_code, status, business_licence, applied_by)
		VALUES ($1, 'Test PLC', 'Test Shop', '5812', 'kyb_review', $2, $3)`, id, lic, owner)
	e.exec(`INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'owner')`, id, owner)
	return id
}

func (e *env) staff() uuid.UUID {
	id := uuid.New()
	e.exec(`INSERT INTO admin.staff (id, email, full_name, sso_subject, status) VALUES ($1, $2, 'Reviewer', $3, 'active')`,
		id, id.String()+"@gebeya.test", "test:"+id.String())
	return id
}

func (e *env) review(doc uuid.UUID, accept bool, note string) (*kyb.Document, error) {
	var d *kyb.Document
	err := pgx.BeginFunc(e.ctx, pool, func(tx pgx.Tx) error {
		var err error
		d, err = e.svc.ReviewTx(e.ctx, tx, doc, e.staff(), accept, note)
		return err
	})
	return d, err
}

func TestUploadChecksAndStoresEncrypted(t *testing.T) {
	e := setup(t)
	owner, cashier, stranger := e.user(), e.user(), e.user()
	shop := e.shop(owner, false)
	e.exec(`INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'cashier')`, shop, cashier)
	photo := kybtest.Photo(uuid.NewString())

	if _, err := e.svc.Upload(e.ctx, cashier, shop, "owner_id", photo); !errors.Is(err, kyb.ErrForbidden) {
		t.Errorf("cashier: %v", err)
	}
	if _, err := e.svc.Upload(e.ctx, stranger, shop, "owner_id", photo); !errors.Is(err, kyb.ErrNotFound) {
		t.Errorf("stranger: %v", err)
	}
	var v *kyb.ValidationError
	for name, data := range map[string][]byte{
		"html":      append([]byte("<html><script>"), make([]byte, 2048)...),
		"too small": []byte{0xFF, 0xD8, 0xFF, 0xE0},
		"too big":   append([]byte{0xFF, 0xD8, 0xFF}, make([]byte, kyb.MaxSize)...),
	} {
		if _, err := e.svc.Upload(e.ctx, owner, shop, "owner_id", data); !errors.As(err, &v) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := e.svc.Upload(e.ctx, owner, shop, "selfie", photo); !errors.As(err, &v) {
		t.Errorf("unknown kind: %v", err)
	}

	d, err := e.svc.Upload(e.ctx, owner, shop, "owner_id", photo)
	if err != nil {
		t.Fatal(err)
	}
	if d.ContentType != "image/jpeg" || d.Status != "submitted" || d.Size != len(photo) {
		t.Errorf("%+v", d)
	}
	// Sending the same file again (a retry) gives the same document.
	again, err := e.svc.Upload(e.ctx, owner, shop, "owner_id", photo)
	if err != nil || again.ID != d.ID || len(e.store.Objects) != 1 {
		t.Fatalf("retry: %v %v %d objects", err, again, len(e.store.Objects))
	}
	// What's stored is encrypted: the photo's bytes don't appear in it.
	for _, blob := range e.store.Objects {
		if bytes.Contains(blob, photo[:64]) {
			t.Error("stored in clear")
		}
	}
	_, got, err := e.svc.Open(e.ctx, d.ID)
	if err != nil || !bytes.Equal(got, photo) {
		t.Fatalf("open: %v", err)
	}

	// A new photo of the same kind replaces the first; the owner sees only the new one.
	d2, err := e.svc.Upload(e.ctx, owner, shop, "owner_id", kybtest.Photo(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	list, err := e.svc.List(e.ctx, owner, shop)
	if err != nil || len(list) != 1 || list[0].ID != d2.ID {
		t.Fatalf("list: %v %+v", err, list)
	}
	if all, _ := e.svc.ForMerchant(e.ctx, shop); len(all) != 2 || all[1].Status != "replaced" {
		t.Errorf("staff view: %+v", all)
	}
	if _, err := e.svc.List(e.ctx, cashier, shop); !errors.Is(err, kyb.ErrForbidden) {
		t.Errorf("cashier list: %v", err)
	}
}

func TestStoredFilesCantBeSwappedOrTamperedWith(t *testing.T) {
	e := setup(t)
	owner := e.user()
	shop := e.shop(owner, false)
	a, _ := e.svc.Upload(e.ctx, owner, shop, "tin_certificate", kybtest.Photo("a"+uuid.NewString()))
	b, _ := e.svc.Upload(e.ctx, owner, shop, "owner_id", kybtest.Photo("b"+uuid.NewString()))
	keyA, keyB := "kyb/"+shop.String()+"/"+a.ID.String(), "kyb/"+shop.String()+"/"+b.ID.String()
	e.store.Objects[keyA], e.store.Objects[keyB] = e.store.Objects[keyB], e.store.Objects[keyA]
	if _, _, err := e.svc.Open(e.ctx, a.ID); err == nil {
		t.Error("a swapped file opened")
	}
	e.store.Objects[keyA], e.store.Objects[keyB] = e.store.Objects[keyB], e.store.Objects[keyA]
	e.store.Objects[keyA][len(e.store.Objects[keyA])-1] ^= 1
	if _, _, err := e.svc.Open(e.ctx, a.ID); err == nil {
		t.Error("a changed file opened")
	}
	if _, _, err := e.svc.Open(e.ctx, b.ID); err != nil {
		t.Errorf("untouched file: %v", err)
	}
}

func TestKeyRotation(t *testing.T) {
	old := keySpec("k1")
	s1, _ := kyb.ParseKeys(old)
	id, blob := s1.Seal("kyb/x/y", []byte("hello"))
	s2, err := kyb.ParseKeys(keySpec("k2") + "," + old)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s2.Open(id, "kyb/x/y", blob); err != nil || string(got) != "hello" {
		t.Errorf("old file with the new key list: %v", err)
	}
	if id2, _ := s2.Seal("kyb/x/z", []byte("x")); id2 != "k2" {
		t.Errorf("new files use %s", id2)
	}
	for _, bad := range []string{"", "k1", "k1:short", "k1:" + base64.StdEncoding.EncodeToString(make([]byte, 16))} {
		if _, err := kyb.ParseKeys(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestReviewAndWhatsMissing(t *testing.T) {
	e := setup(t)
	owner := e.user()
	shop := e.shop(owner, true)
	missing := func() []string {
		m, err := kyb.MissingTx(e.ctx, pool, shop)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := missing(); !slices.Equal(m, []string{"tin_certificate", "trade_licence", "owner_id"}) {
		t.Errorf("missing %v", m)
	}
	if r, _ := kyb.RequiredTx(e.ctx, pool, e.shop(owner, false)); !slices.Equal(r, []string{"tin_certificate", "owner_id"}) {
		t.Errorf("required without a licence: %v", r)
	}

	tin, _ := e.svc.Upload(e.ctx, owner, shop, "tin_certificate", kybtest.Photo(uuid.NewString()))
	id, _ := e.svc.Upload(e.ctx, owner, shop, "owner_id", kybtest.Photo(uuid.NewString()))
	if _, err := e.review(tin.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	var v *kyb.ValidationError
	if _, err := e.review(tin.ID, false, "Blurry, can't read the number"); !errors.As(err, &v) {
		t.Errorf("deciding twice: %v", err)
	}
	if _, err := e.review(id.ID, false, "blurry"); !errors.As(err, &v) {
		t.Errorf("rejecting without a real reason: %v", err)
	}
	d, err := e.review(id.ID, false, "The photo is blurry; the ID number can't be read")
	if err != nil || d.Status != "rejected" || d.ReviewNote == "" {
		t.Fatalf("reject: %v %+v", err, d)
	}
	var payload []byte
	_ = pool.QueryRow(e.ctx, `SELECT payload FROM platform.outbox WHERE topic = 'merchant.document_rejected' AND aggregate_id = $1`,
		shop).Scan(&payload)
	var ev map[string]any
	_ = json.Unmarshal(payload, &ev)
	if ev["document"] != "owner_id" || ev["reason"] != "The photo is blurry; the ID number can't be read" {
		t.Errorf("outbox: %s", payload)
	}
	if m := missing(); !slices.Equal(m, []string{"trade_licence", "owner_id"}) {
		t.Errorf("missing %v", m)
	}
	// The owner sends a better photo: the rejected one is replaced and the new one waits for review.
	id2, _ := e.svc.Upload(e.ctx, owner, shop, "owner_id", kybtest.Photo(uuid.NewString()))
	if list, _ := e.svc.List(e.ctx, owner, shop); len(list) != 2 || !slices.ContainsFunc(list, func(d kyb.Document) bool { return d.ID == id2.ID }) {
		t.Errorf("after replacing: %+v", list)
	}
}

// TestS3 runs against a real S3-compatible store when TEST_S3_ENDPOINT is set (MinIO in development).
func TestS3(t *testing.T) {
	ep := os.Getenv("TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("TEST_S3_ENDPOINT not set")
	}
	s, err := kyb.NewS3(kyb.S3Config{Endpoint: ep, Bucket: "g1-kyb-test", AccessKey: os.Getenv("TEST_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("TEST_S3_SECRET_KEY"), Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	key := "kyb/test/" + uuid.NewString()
	if err := s.Put(ctx, key, []byte("sealed bytes")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(ctx, key); err != nil || string(got) != "sealed bytes" {
		t.Fatalf("get: %v %q", err, got)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, key); err == nil {
		t.Error("deleted object still readable")
	}
}
