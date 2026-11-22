package keyattest_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"superapp/backend/internal/keyattest"
	kt "superapp/backend/internal/keyattest/keyattesttest"
)

func deviceKey() *ecdsa.PrivateKey {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return k
}

func TestAGoodChainIsAccepted(t *testing.T) {
	a, key := kt.New(t), deviceKey()
	ch := []byte("challenge-1")
	r, err := a.Verifier().Verify(context.Background(), a.Chain(&key.PublicKey, kt.Good(ch)), ch)
	if err != nil {
		t.Fatal(err)
	}
	if r.Level() != "tee" || !r.PublicKey.Equal(&key.PublicKey) || r.VerifiedBoot != keyattest.BootVerified || !r.DeviceLocked ||
		r.OSPatchLevel != 202510 || r.UserAuthBiometric || r.RevocationChecked {
		t.Errorf("result: %+v", r)
	}
	d := kt.Good(ch)
	d.Level, d.Biometric, d.BootState, d.Locked, d.ExtraTag = keyattest.StrongBox, true, 2, false, true
	r, err = a.Verifier().Verify(context.Background(), a.Chain(&key.PublicKey, d), ch)
	if err != nil || r.Level() != "strongbox" || !r.UserAuthBiometric || r.VerifiedBoot != keyattest.BootUnverified || r.DeviceLocked {
		t.Errorf("strongbox, biometric, unlocked: %+v %v", r, err)
	}
}

func TestBadChainsAreRefused(t *testing.T) {
	a, key := kt.New(t), deviceKey()
	pub := &key.PublicKey
	ch := []byte("challenge-2")
	with := func(f func(*kt.Desc)) [][]byte { d := kt.Good(ch); f(&d); return a.Chain(pub, d) }
	cases := map[string]func() [][]byte{
		"software key":    func() [][]byte { return with(func(d *kt.Desc) { d.Level = keyattest.Software }) },
		"other challenge": func() [][]byte { return with(func(d *kt.Desc) { d.Challenge = []byte("old") }) },
		"RSA key":         func() [][]byte { return with(func(d *kt.Desc) { d.Algorithm = 1 }) },
		"imported key":    func() [][]byte { return with(func(d *kt.Desc) { d.Origin = 2 }) },
		"other app":       func() [][]byte { return with(func(d *kt.Desc) { d.Package = "com.example.clone" }) },
		"other signer":    func() [][]byte { return with(func(d *kt.Desc) { d.Signer = make([]byte, 32) }) },
		"leaf only":       func() [][]byte { return a.Chain(pub, kt.Good(ch))[:1] },
		"no root":         func() [][]byte { return a.Chain(pub, kt.Good(ch))[:2] },
		"another root":    func() [][]byte { return kt.New(t).Chain(pub, kt.Good(ch)) },
		"broken signature": func() [][]byte {
			c := a.Chain(pub, kt.Good(ch))
			c[0][len(c[0])-3] ^= 0xff
			return c
		},
		"intermediate expired": func() [][]byte {
			old, oldD, oldKey := a.IssueUnderRoot(true, time.Now().Add(-time.Minute), nil)
			_, leaf, _ := kt.Issue(t, old, oldKey, pub, false, time.Now().Add(time.Hour), kt.Good(ch).Extension())
			return [][]byte{leaf, oldD, a.RootDER}
		},
		"extension on an intermediate": func() [][]byte {
			mid, midD, midKey := a.IssueUnderRoot(true, time.Now().Add(time.Hour), kt.Good(ch).Extension())
			_, leaf, _ := kt.Issue(t, mid, midKey, pub, false, time.Now().Add(time.Hour), kt.Good(ch).Extension())
			return [][]byte{leaf, midD, a.RootDER}
		},
		"no extension": func() [][]byte {
			mid, midD, midKey := a.IssueUnderRoot(true, time.Now().Add(time.Hour), nil)
			_, leaf, _ := kt.Issue(t, mid, midKey, pub, false, time.Now().Add(time.Hour), nil)
			return [][]byte{leaf, midD, a.RootDER}
		},
		"garbage": func() [][]byte { return [][]byte{[]byte("x"), []byte("y")} },
	}
	v := a.Verifier()
	for name, chain := range cases {
		if _, err := v.Verify(context.Background(), chain(), ch); !errors.Is(err, keyattest.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRevokedCertificatesAreRefused(t *testing.T) {
	a, key := kt.New(t), deviceKey()
	ch := []byte("challenge-3")
	chain := a.Chain(&key.PublicKey, kt.Good(ch))
	inter, _ := x509.ParseCertificate(a.InterDER)
	hits, fail := 0, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if fail {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, `{"entries":{"%s":{"status":"REVOKED","reason":"KEY_COMPROMISE"},"ffff":{"status":"SUSPENDED"}}}`,
			strings.ToUpper(inter.SerialNumber.Text(16)))
	}))
	defer srv.Close()
	now := time.Now()
	status := &keyattest.StatusList{URL: srv.URL, Now: func() time.Time { return now }}
	v := a.Verifier()
	v.Status = status
	if _, err := v.Verify(context.Background(), chain, ch); !errors.Is(err, keyattest.ErrInvalid) {
		t.Fatalf("revoked intermediate: %v", err)
	}
	if r, _ := status.Revoked(context.Background(), "FFFF"); !r || hits != 1 {
		t.Errorf("suspended or cached: %v %d", r, hits)
	}

	// A fresh list is fetched after MaxAge; if that fails, the old copy is kept.
	now = now.Add(13 * time.Hour)
	fail = true
	if r, err := status.Revoked(context.Background(), "ffff"); !r || err != nil || hits != 2 {
		t.Errorf("stale copy: %v %v %d", r, err, hits)
	}

	// Never fetched and unreachable: the chain is still checked, just not against the list.
	b := kt.New(t)
	v2 := b.Verifier()
	v2.Status = &keyattest.StatusList{URL: srv.URL}
	r, err := v2.Verify(context.Background(), b.Chain(&key.PublicKey, kt.Good(ch)), ch)
	if err != nil || r.RevocationChecked {
		t.Errorf("unreachable list: %+v %v", r, err)
	}
}

func TestGoogleRootsAreEmbedded(t *testing.T) {
	roots := keyattest.GoogleRoots()
	if len(roots) != 2 {
		t.Fatalf("%d roots", len(roots))
	}
	if _, ok := roots[1].(*ecdsa.PublicKey); !ok {
		t.Errorf("second root isn't the ECDSA CA1: %T", roots[1])
	}
}

// TestGoogleStatusListLive fetches Google's real revocation list. Opt-in: KEYATTEST_LIVE=1.
func TestGoogleStatusListLive(t *testing.T) {
	if os.Getenv("KEYATTEST_LIVE") != "1" {
		t.Skip("KEYATTEST_LIVE not set")
	}
	revoked, err := (&keyattest.StatusList{}).Revoked(context.Background(), "0")
	if err != nil || revoked {
		t.Fatalf("status list: %v %v", revoked, err)
	}
}
