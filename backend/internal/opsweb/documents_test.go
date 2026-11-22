package opsweb_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"superapp/backend/internal/kyb"
	"superapp/backend/internal/kyb/kybtest"
)

func TestDocumentsInTheConsole(t *testing.T) {
	e := setup(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	sealer, _ := kyb.ParseKeys("k1:" + base64.StdEncoding.EncodeToString(key))
	e.svc.KYB = &kyb.Service{Pool: pool, Store: kybtest.NewMemStore(), Sealer: sealer}
	ctx := context.Background()

	owner, shop := uuid.New(), uuid.New()
	_, _ = pool.Exec(ctx, `INSERT INTO identity.users (id, phone_e164, status) VALUES ($1, $2, 'active')`,
		owner, fmt.Sprintf("+2519%08d", binary.BigEndian.Uint64(owner[:8])%100_000_000))
	_, _ = pool.Exec(ctx, `INSERT INTO merchant.merchants (id, legal_name, trade_name, category_code, city, status, applied_by)
		VALUES ($1, 'Abay PLC', 'Abay Kiosk', '5411', 'Bahir Dar', 'kyb_review', $2)`, shop, owner)
	_, _ = pool.Exec(ctx, `INSERT INTO merchant.members (merchant_id, user_id, role) VALUES ($1, $2, 'owner')`, shop, owner)
	photo := kybtest.Photo(uuid.NewString())
	tin, err := e.svc.KYB.Upload(ctx, owner, shop, "tin_certificate", photo)
	if err != nil {
		t.Fatal(err)
	}
	pdf := append([]byte("%PDF-1.4\n"), make([]byte, 2048)...)
	copy(pdf[9:], uuid.NewString())
	id, err := e.svc.KYB.Upload(ctx, owner, shop, "owner_id", pdf)
	if err != nil {
		t.Fatal(err)
	}

	b, _, _ := e.signIn(claimsFor(e.invite("Meron", "merchant_ops")))
	page := "/ops/merchants/" + shop.String()
	_, html := b.do("GET", page, nil, "")
	if !strings.Contains(html, "Still to accept before approval: TIN certificate, Owner&#39;s ID") ||
		!strings.Contains(html, "/ops/documents/"+tin.ID.String()) || !strings.Contains(html, "(PDF download)") {
		t.Fatalf("documents not listed:\n%s", html)
	}
	csrf := csrfRE.FindStringSubmatch(html)[1]

	res, body := b.do("GET", "/ops/documents/"+tin.ID.String(), nil, "")
	if res.StatusCode != http.StatusOK || body != string(photo) || res.Header.Get("Content-Type") != "image/jpeg" ||
		!strings.Contains(res.Header.Get("Content-Security-Policy"), "sandbox") ||
		!strings.HasPrefix(res.Header.Get("Content-Disposition"), "inline") || res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("photo: %d %v", res.StatusCode, res.Header)
	}
	res, _ = b.do("GET", "/ops/documents/"+id.ID.String(), nil, "")
	if !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("PDF shown inline: %v", res.Header)
	}
	if res, _ := b.do("GET", "/ops/documents/"+uuid.NewString(), nil, ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown document: %d", res.StatusCode)
	}

	review := func(doc uuid.UUID, decision, note string) string {
		res, _ := b.do("POST", "/ops/documents/"+doc.String()+"/review", url.Values{"csrf": {csrf}, "decision": {decision}, "note": {note}}, "")
		return res.Header.Get("Location")
	}
	if loc := review(id.ID, "reject", "blurry"); loc != page+"?msg=doc_note" {
		t.Errorf("reject without a reason: %s", loc)
	}
	if loc := review(tin.ID, "accept", ""); loc != page+"?msg=doc_accepted" {
		t.Errorf("accept: %s", loc)
	}
	if loc := review(tin.ID, "accept", ""); loc != page+"?msg=doc_decided" {
		t.Errorf("accept twice: %s", loc)
	}
	if loc := review(id.ID, "reject", "The ID photo is cut off at the bottom"); loc != page+"?msg=doc_rejected" {
		t.Errorf("reject: %s", loc)
	}
	res, _ = b.do("POST", page+"/action", url.Values{"csrf": {csrf}, "kind": {"merchant.approve"},
		"note": {"Checked the TIN with the Ministry of Revenues"}}, "")
	if loc := res.Header.Get("Location"); loc != page+"?msg=conflict" {
		t.Errorf("approve with the ID rejected: %s", loc)
	}
	_, html = b.do("GET", page, nil, "")
	for _, want := range []string{"Still to accept before approval: Owner&#39;s ID", "The ID photo is cut off at the bottom",
		"Opened a document", "Rejected a document", "Didn&#39;t go through"} {
		if !strings.Contains(html, want) {
			t.Errorf("merchant page lacks %q", want)
		}
	}
	if strings.Contains(e.logs.String(), "level=ERROR") {
		t.Errorf("server errors: %s", e.logs.String())
	}
}
