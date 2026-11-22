// Package keyattest checks Android key attestation: proof, signed by Google's attestation roots,
// that a key was generated inside a phone's secure hardware (TEE or StrongBox) and can't leave it.
//
// The app creates its DPoP key (and its biometric key) with an attestation challenge the server can
// recompute, and sends the certificate chain the Android Keystore returns. Verify checks:
//   - every certificate is signed by the next, and the last one carries a Google root public key
//     (by key, so the re-issued copies of the RSA root all work);
//   - no certificate in the chain is revoked or suspended on Google's status list;
//   - only the leaf carries the attestation extension, and it says the key lives in hardware;
//   - the challenge matches, so a chain from another sign-in can't be replayed;
//   - the key is a P-256 signing key generated on the device (not imported);
//   - the app is ours (package name and, when configured, the signing certificate).
//
// The verified-boot state and patch level are reported, not judged: a phone with an unlocked
// bootloader still keeps the key in hardware, and what to do about it is a risk decision.
//
// Reference: https://developer.android.com/privacy-and-security/security-key-attestation
package keyattest

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"

	_ "embed"
)

// OID of the KeyDescription extension in the leaf certificate.
var oidKeyDescription = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 1, 17}

// Security levels from the KeyDescription.
const (
	Software   = 0
	TEE        = 1 // TrustedEnvironment
	StrongBox  = 2
	maxChain   = 10
	maxCertLen = 16 << 10
)

// Verified boot states.
const (
	BootVerified   = "verified"
	BootSelfSigned = "self_signed"
	BootUnverified = "unverified"
	BootFailed     = "failed"
)

var ErrInvalid = errors.New("key attestation invalid")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

//go:embed google_roots.pem
var googleRootsPEM []byte

// GoogleRoots returns the public keys of Google's hardware attestation roots, from
// https://android.googleapis.com/attestation/root: the RSA root used since 2016 and
// "Key Attestation CA1" (ECDSA P-384), which signs chains from 1 February 2026.
func GoogleRoots() []crypto.PublicKey {
	var out []crypto.PublicKey
	rest := googleRootsPEM
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			return out
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			panic("keyattest: bad embedded root: " + err.Error())
		}
		out = append(out, c.PublicKey)
	}
}

// Result is what a verified attestation says about the key and the phone.
type Result struct {
	SecurityLevel     int // TEE or StrongBox
	PublicKey         *ecdsa.PublicKey
	VerifiedBoot      string
	DeviceLocked      bool
	OSPatchLevel      int  // YYYYMM, 0 if not reported
	UserAuthBiometric bool // the key needs a biometric check for every use (userAuthType has the biometric bit, no noAuthRequired)
	RevocationChecked bool // false when Google's status list couldn't be fetched
}

// Level is "strongbox" or "tee".
func (r *Result) Level() string {
	if r.SecurityLevel == StrongBox {
		return "strongbox"
	}
	return "tee"
}

// Revocations tells whether a certificate serial (lowercase hex, as Google's status list keys them)
// is revoked or suspended.
type Revocations interface {
	Revoked(ctx context.Context, serial string) (bool, error)
}

type Verifier struct {
	Roots         []crypto.PublicKey
	Status        Revocations // nil: not checked
	Package       string      // the Android application ID, e.g. one.gebeya.gebeya_one
	SignerDigests [][]byte    // SHA-256 of the app signing certificates allowed; empty: not checked
	Now           func() time.Time
}

// Verify checks a certificate chain (leaf first, DER) for a key created with [challenge].
func (v *Verifier) Verify(ctx context.Context, chain [][]byte, challenge []byte) (*Result, error) {
	if len(chain) < 2 || len(chain) > maxChain {
		return nil, invalid("chain has %d certificates", len(chain))
	}
	certs := make([]*x509.Certificate, len(chain))
	for i, der := range chain {
		if len(der) > maxCertLen {
			return nil, invalid("certificate %d too large", i)
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, invalid("certificate %d: %v", i, err)
		}
		certs[i] = c
	}
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	for i := 0; i+1 < len(certs); i++ {
		// CheckSignature, not CheckSignatureFrom: some batch attestation certificates on older phones
		// lack the CA basic constraint, and trust here comes from the chain ending at a pinned root key.
		if err := certs[i+1].CheckSignature(certs[i].SignatureAlgorithm, certs[i].RawTBSCertificate, certs[i].Signature); err != nil {
			return nil, invalid("certificate %d isn't signed by the next: %v", i, err)
		}
		// Intermediates must be in date. The leaf's dates come from the key's own parameters, and the
		// root is trusted by its key (the first RSA root certificate expired in May 2026; phones still carry it).
		if i > 0 && (now.Before(certs[i].NotBefore) || now.After(certs[i].NotAfter)) {
			return nil, invalid("certificate %d is out of date", i)
		}
	}
	if !v.trusted(certs[len(certs)-1].PublicKey) {
		return nil, invalid("chain doesn't end at a Google attestation root")
	}
	res := &Result{}
	if v.Status != nil {
		res.RevocationChecked = true
		for i, c := range certs {
			revoked, err := v.Status.Revoked(ctx, c.SerialNumber.Text(16))
			if err != nil {
				res.RevocationChecked = false
				break
			}
			if revoked {
				return nil, invalid("certificate %d is revoked", i)
			}
		}
	}

	var ext []byte
	for i, c := range certs {
		for _, e := range c.Extensions {
			if e.Id.Equal(oidKeyDescription) {
				if i != 0 {
					return nil, invalid("attestation extension outside the leaf")
				}
				ext = e.Value
			}
		}
	}
	if ext == nil {
		return nil, invalid("no attestation extension")
	}
	kd, err := parseKeyDescription(ext)
	if err != nil {
		return nil, err
	}
	if kd.attestationLevel < TEE || kd.keyMintLevel < TEE {
		return nil, invalid("key isn't in secure hardware (level %d/%d)", kd.attestationLevel, kd.keyMintLevel)
	}
	if !bytes.Equal(kd.challenge, challenge) {
		return nil, invalid("challenge doesn't match")
	}
	hw := kd.hardware
	if hw.algorithm != 3 || hw.ecCurve != 1 || !hw.purposes[2] {
		return nil, invalid("not a hardware P-256 signing key")
	}
	if !hw.hasOrigin || hw.origin != 0 {
		return nil, invalid("key wasn't generated on the device")
	}
	pub, ok := certs[0].PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, invalid("leaf key isn't P-256")
	}
	if err := v.checkApp(kd); err != nil {
		return nil, err
	}

	res.SecurityLevel = kd.attestationLevel
	res.PublicKey = pub
	res.OSPatchLevel = hw.osPatchLevel
	res.UserAuthBiometric = hw.userAuthType&2 != 0 && !hw.noAuthRequired
	res.VerifiedBoot = BootFailed
	if hw.rootOfTrust != nil {
		res.DeviceLocked = hw.rootOfTrust.DeviceLocked
		switch hw.rootOfTrust.VerifiedBootState {
		case 0:
			res.VerifiedBoot = BootVerified
		case 1:
			res.VerifiedBoot = BootSelfSigned
		case 2:
			res.VerifiedBoot = BootUnverified
		}
	}
	return res, nil
}

func (v *Verifier) trusted(k crypto.PublicKey) bool {
	got, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		return false
	}
	for _, r := range v.Roots {
		want, err := x509.MarshalPKIXPublicKey(r)
		if err == nil && bytes.Equal(got, want) {
			return true
		}
	}
	return false
}

// checkApp matches attestationApplicationId (software-enforced: the Keystore fills it from the
// package manager) against our package and signing certificates.
func (v *Verifier) checkApp(kd *keyDescription) error {
	if v.Package == "" && len(v.SignerDigests) == 0 {
		return nil
	}
	raw := kd.software.applicationID
	if raw == nil {
		raw = kd.hardware.applicationID
	}
	if raw == nil {
		return invalid("no application ID")
	}
	var app struct {
		Packages []struct {
			Name    []byte
			Version int64
		} `asn1:"set"`
		Digests [][]byte `asn1:"set"`
	}
	if rest, err := asn1.Unmarshal(raw, &app); err != nil || len(rest) != 0 {
		return invalid("application ID: %v", err)
	}
	if v.Package != "" {
		found := false
		for _, p := range app.Packages {
			found = found || string(p.Name) == v.Package
		}
		if !found {
			return invalid("key belongs to another app")
		}
	}
	if len(v.SignerDigests) > 0 {
		for _, d := range app.Digests {
			for _, w := range v.SignerDigests {
				if bytes.Equal(d, w) {
					return nil
				}
			}
		}
		return invalid("app isn't signed with our certificate")
	}
	return nil
}

// ---------------------------------------------------------------------------
// KeyDescription parsing
// ---------------------------------------------------------------------------

type rootOfTrust struct {
	VerifiedBootKey   []byte
	DeviceLocked      bool
	VerifiedBootState asn1.Enumerated
	VerifiedBootHash  []byte `asn1:"optional"`
}

type authorizations struct {
	purposes       map[int]bool
	algorithm      int
	ecCurve        int
	noAuthRequired bool
	userAuthType   int
	origin         int
	hasOrigin      bool
	rootOfTrust    *rootOfTrust
	osPatchLevel   int
	applicationID  []byte
}

type keyDescription struct {
	attestationLevel int
	keyMintLevel     int
	challenge        []byte
	software         authorizations
	hardware         authorizations
}

// parseKeyDescription reads the fields we use. AuthorizationList is a long SEQUENCE of optional
// fields with high context tags that grows with every KeyMint version, so it's walked element by
// element and unknown tags are skipped.
func parseKeyDescription(der []byte) (*keyDescription, error) {
	var seq asn1.RawValue
	if rest, err := asn1.Unmarshal(der, &seq); err != nil || len(rest) != 0 || seq.Tag != asn1.TagSequence {
		return nil, invalid("KeyDescription isn't a sequence")
	}
	var el []asn1.RawValue
	for rest := seq.Bytes; len(rest) > 0; {
		var r asn1.RawValue
		var err error
		if rest, err = asn1.Unmarshal(rest, &r); err != nil {
			return nil, invalid("KeyDescription: %v", err)
		}
		el = append(el, r)
	}
	if len(el) < 8 {
		return nil, invalid("KeyDescription has %d fields", len(el))
	}
	kd := &keyDescription{}
	var err error
	if kd.attestationLevel, err = smallInt(el[1], asn1.TagEnum); err != nil {
		return nil, err
	}
	if kd.keyMintLevel, err = smallInt(el[3], asn1.TagEnum); err != nil {
		return nil, err
	}
	if el[4].Tag != asn1.TagOctetString || el[4].Class != asn1.ClassUniversal {
		return nil, invalid("challenge isn't an octet string")
	}
	kd.challenge = el[4].Bytes
	if kd.software, err = parseAuthorizations(el[6]); err != nil {
		return nil, err
	}
	if kd.hardware, err = parseAuthorizations(el[7]); err != nil {
		return nil, err
	}
	return kd, nil
}

func parseAuthorizations(list asn1.RawValue) (authorizations, error) {
	a := authorizations{purposes: map[int]bool{}}
	if list.Tag != asn1.TagSequence || list.Class != asn1.ClassUniversal {
		return a, invalid("AuthorizationList isn't a sequence")
	}
	seen := map[int]bool{}
	for rest := list.Bytes; len(rest) > 0; {
		var field asn1.RawValue
		var err error
		if rest, err = asn1.Unmarshal(rest, &field); err != nil {
			return a, invalid("AuthorizationList: %v", err)
		}
		if field.Class != asn1.ClassContextSpecific || !field.IsCompound {
			return a, invalid("AuthorizationList field isn't tagged")
		}
		if seen[field.Tag] {
			return a, invalid("AuthorizationList repeats tag %d", field.Tag)
		}
		seen[field.Tag] = true
		var inner asn1.RawValue
		if r, err := asn1.Unmarshal(field.Bytes, &inner); err != nil || len(r) != 0 {
			return a, invalid("AuthorizationList tag %d: bad value", field.Tag)
		}
		switch field.Tag {
		case 1: // purpose
			if inner.Tag != asn1.TagSet {
				return a, invalid("purpose isn't a set")
			}
			for r := inner.Bytes; len(r) > 0; {
				var p asn1.RawValue
				if r, err = asn1.Unmarshal(r, &p); err != nil {
					return a, invalid("purpose: %v", err)
				}
				n, err := smallInt(p, asn1.TagInteger)
				if err != nil {
					return a, err
				}
				a.purposes[n] = true
			}
		case 2:
			a.algorithm, err = smallInt(inner, asn1.TagInteger)
		case 10:
			a.ecCurve, err = smallInt(inner, asn1.TagInteger)
		case 503:
			a.noAuthRequired = inner.Tag == asn1.TagNull
		case 504:
			a.userAuthType, err = smallInt(inner, asn1.TagInteger)
		case 702:
			a.origin, err = smallInt(inner, asn1.TagInteger)
			a.hasOrigin = true
		case 704:
			var rot rootOfTrust
			if r, e := asn1.Unmarshal(field.Bytes, &rot); e != nil || len(r) != 0 {
				return a, invalid("rootOfTrust: %v", e)
			}
			a.rootOfTrust = &rot
		case 706:
			a.osPatchLevel, err = smallInt(inner, asn1.TagInteger)
		case 709:
			if inner.Tag != asn1.TagOctetString {
				return a, invalid("attestationApplicationId isn't an octet string")
			}
			a.applicationID = inner.Bytes
		}
		if err != nil {
			return a, err
		}
	}
	return a, nil
}

// smallInt reads a non-negative INTEGER or ENUMERATED that fits in an int.
func smallInt(v asn1.RawValue, tag int) (int, error) {
	if v.Class != asn1.ClassUniversal || v.Tag != tag || len(v.Bytes) == 0 || len(v.Bytes) > 8 {
		return 0, invalid("expected a small integer (tag %d)", tag)
	}
	n := new(big.Int).SetBytes(v.Bytes)
	if v.Bytes[0]&0x80 != 0 || !n.IsInt64() {
		return 0, invalid("integer out of range")
	}
	return int(n.Int64()), nil
}
