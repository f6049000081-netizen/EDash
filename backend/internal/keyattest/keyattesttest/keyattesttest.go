// Package keyattesttest builds Android-Keystore-style attestation chains under a test root, for
// tests only. Production trusts only Google's roots (keyattest.GoogleRoots), so these chains are
// refused there.
package keyattesttest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"superapp/backend/internal/keyattest"
)

const Package = "one.gebeya.gebeya_one"

// Signer is the SHA-256 of the "app signing certificate" test chains carry by default.
var Signer = sha256.Sum256([]byte("test app signing cert"))

// Desc is what a chain's KeyDescription says.
type Desc struct {
	Level      int // keyattest.Software, TEE or StrongBox
	Challenge  []byte
	Algorithm  int // 3 = EC
	Origin     int // 0 = generated on the device
	Package    string
	Signer     []byte
	BootState  int // 0 verified, 1 self-signed, 2 unverified
	Locked     bool
	Biometric  bool // userAuthType biometric instead of noAuthRequired
	ExtraTag   bool // an AuthorizationList tag the verifier doesn't know
	PatchLevel int
}

// Good is a TEE key generated on a locked, verified phone, for our app.
func Good(challenge []byte) Desc {
	return Desc{Level: keyattest.TEE, Challenge: challenge, Algorithm: 3, Package: Package, Signer: Signer[:],
		Locked: true, PatchLevel: 202510}
}

func explicit(tag int, v any) asn1.RawValue {
	return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: tag, IsCompound: true, Bytes: must(asn1.Marshal(v))}
}

func seq(fields ...asn1.RawValue) asn1.RawValue {
	var body []byte
	for _, f := range fields {
		body = append(body, must(asn1.Marshal(f))...)
	}
	return asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: body}
}

func raw(v any) asn1.RawValue {
	var r asn1.RawValue
	if _, err := asn1.Unmarshal(must(asn1.Marshal(v)), &r); err != nil {
		panic(err)
	}
	return r
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// Extension is the DER KeyDescription.
func (d Desc) Extension() []byte {
	type pkgInfo struct {
		Name    []byte
		Version int64
	}
	app := must(asn1.Marshal(struct {
		Packages []pkgInfo `asn1:"set"`
		Digests  [][]byte  `asn1:"set"`
	}{[]pkgInfo{{[]byte(d.Package), 7}}, [][]byte{d.Signer}}))
	purposes := asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSet, IsCompound: true,
		Bytes: append(must(asn1.Marshal(2)), must(asn1.Marshal(7))...)}
	hw := []asn1.RawValue{explicit(1, purposes), explicit(2, d.Algorithm), explicit(3, 256), explicit(10, 1)}
	if d.ExtraTag {
		hw = append(hw, explicit(400, 5))
	}
	if d.Biometric {
		hw = append(hw, explicit(504, 2))
	} else {
		hw = append(hw, asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 503, IsCompound: true, Bytes: asn1.NullBytes})
	}
	hw = append(hw, explicit(702, d.Origin), explicit(704, struct {
		Key    []byte
		Locked bool
		State  asn1.Enumerated
		Hash   []byte
	}{make([]byte, 32), d.Locked, asn1.Enumerated(d.BootState), make([]byte, 32)}),
		explicit(705, 140000), explicit(706, d.PatchLevel))
	return must(asn1.Marshal(seq(
		raw(300), raw(asn1.Enumerated(d.Level)), raw(300), raw(asn1.Enumerated(d.Level)),
		raw(d.Challenge), raw([]byte{}),
		seq(explicit(709, app)), seq(hw...),
	)))
}

// Authority is a test root with an intermediate, standing in for Google's attestation PKI.
type Authority struct {
	t        *testing.T
	Root     *x509.Certificate
	RootDER  []byte
	rootKey  *ecdsa.PrivateKey
	Inter    *x509.Certificate
	InterDER []byte
	interKey *ecdsa.PrivateKey
}

var serial atomic.Int64

// Issue signs a certificate for [pub] (a new key when nil) with [parent] (self-signed when nil).
func Issue(t *testing.T, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, pub crypto.PublicKey, isCA bool,
	notAfter time.Time, ext []byte) (*x509.Certificate, []byte, *ecdsa.PrivateKey) {
	t.Helper()
	n := serial.Add(1) + time.Now().UnixNano()%1_000_000_000*1000
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(n), Subject: pkix.Name{CommonName: "test attestation"},
		NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: notAfter, BasicConstraintsValid: isCA, IsCA: isCA}
	if ext != nil {
		tmpl.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 1, 17}, Value: ext}}
	}
	var key *ecdsa.PrivateKey
	if pub == nil {
		key, _ = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		pub = &key.PublicKey
	}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c, der, key
}

// New makes a root (already expired: trust is by key) and a current intermediate.
func New(t *testing.T) *Authority {
	a := &Authority{t: t}
	a.Root, a.RootDER, a.rootKey = Issue(t, nil, nil, nil, true, time.Now().Add(-24*time.Hour), nil)
	a.Inter, a.InterDER, a.interKey = Issue(t, a.Root, a.rootKey, nil, true, time.Now().Add(365*24*time.Hour), nil)
	return a
}

// IssueUnderRoot signs a certificate directly with the root (for intermediates a test makes itself).
func (a *Authority) IssueUnderRoot(isCA bool, notAfter time.Time, ext []byte) (*x509.Certificate, []byte, *ecdsa.PrivateKey) {
	return Issue(a.t, a.Root, a.rootKey, nil, isCA, notAfter, ext)
}

// Chain attests [pub] with [d]: leaf, intermediate, root.
func (a *Authority) Chain(pub *ecdsa.PublicKey, d Desc) [][]byte {
	_, leaf, _ := Issue(a.t, a.Inter, a.interKey, pub, false, time.Now().Add(time.Hour), d.Extension())
	return [][]byte{leaf, a.InterDER, a.RootDER}
}

// Verifier trusts this authority's root, for our package and the test signer.
func (a *Authority) Verifier() *keyattest.Verifier {
	return &keyattest.Verifier{Roots: []crypto.PublicKey{a.Root.PublicKey}, Package: Package, SignerDigests: [][]byte{Signer[:]}}
}
