package identity

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"slices"

	"superapp/backend/internal/keyattest"
)

// Key attestation modes (Config.KeyAttestation). They apply to Android; iOS has no device keys yet.
const (
	AttestOff     = "off"     // chains are ignored
	AttestRecord  = "record"  // chains are checked and the outcome kept on the device, never refused
	AttestRequire = "require" // Android sign-ins need a DPoP key proven to be in secure hardware, and
	// fingerprint payments a biometric key proven the same way
)

// Outcomes kept in identity.devices.key_attestation and biometric_key_attestation.
const (
	KeyStrongBox = "strongbox"
	KeyTEE       = "tee"
	KeyNone      = "none"   // no chain sent (older app, or Android 6, which can't attest)
	KeyFailed    = "failed" // a chain that didn't verify
)

// KeyAttester checks an Android key attestation chain (keyattest.Verifier).
type KeyAttester interface {
	Verify(ctx context.Context, chain [][]byte, challenge []byte) (*keyattest.Result, error)
}

// DPoPAttestChallenge is the attestation challenge the app sets on the DPoP key it creates for a
// sign-in: tied to the code's challenge ID, so a chain can't be reused for another sign-in.
func DPoPAttestChallenge(challengeID string) []byte {
	sum := sha256.Sum256([]byte("gebeya-one/dpop-attest/v1:" + challengeID))
	return sum[:]
}

// BiometricAttestChallenge is the challenge for the biometric key: tied to the session's DPoP key
// thumbprint, which only this phone's hardware can use.
func BiometricAttestChallenge(jkt string) []byte {
	sum := sha256.Sum256([]byte("gebeya-one/biometric-attest/v1:" + jkt))
	return sum[:]
}

// dpopRequired tells whether sign-ins and refreshes from [platform] must bind a device key.
func (s *Service) dpopRequired(platform string) bool {
	return slices.Contains(s.Cfg.DPoPRequired, platform) || (platform == "android" && s.Cfg.KeyAttestation == AttestRequire)
}

// keyCheck is the outcome of checking one key's attestation. Outcome "" means it wasn't checked.
type keyCheck struct {
	Outcome string
	Result  *keyattest.Result
}

// checkKey verifies [chain] for a key that must equal [want], and applies the mode: in require mode
// anything but a verified hardware key is ErrAttestationFailed.
func (s *Service) checkKey(ctx context.Context, platform string, chain [][]byte, challenge []byte,
	matches func(*ecdsa.PublicKey) bool, biometric bool) (keyCheck, error) {
	mode := s.Cfg.KeyAttestation
	if mode == "" || mode == AttestOff || platform != "android" || s.KeyAttester == nil {
		return keyCheck{}, nil
	}
	kc := keyCheck{Outcome: KeyNone}
	if len(chain) > 0 && matches != nil {
		res, err := s.KeyAttester.Verify(ctx, chain, challenge)
		switch {
		case err != nil:
			s.Log.Info("key attestation refused", "err", err)
			kc.Outcome = KeyFailed
		case !matches(res.PublicKey):
			s.Log.Info("key attestation is for another key")
			kc.Outcome = KeyFailed
		case biometric && !res.UserAuthBiometric:
			s.Log.Info("biometric key doesn't require a biometric check")
			kc.Outcome = KeyFailed
		default:
			kc = keyCheck{Outcome: res.Level(), Result: res}
		}
	}
	if mode == AttestRequire && kc.Result == nil {
		return kc, ErrAttestationFailed
	}
	return kc, nil
}

// jktOf is the RFC 7638 thumbprint of a P-256 public key, as DPoP binds it.
func jktOf(k *ecdsa.PublicKey) string {
	var x, y [32]byte
	k.X.FillBytes(x[:])
	k.Y.FillBytes(y[:])
	return thumbprint(dpopJWK{X: b64.EncodeToString(x[:]), Y: b64.EncodeToString(y[:])})
}

// spkiOf is the key as ParseBiometricKey stores it (base64 SPKI).
func spkiOf(k *ecdsa.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(der)
}

// attestationColumns are the device columns for a DPoP key check, or all nil when it wasn't checked.
func (kc keyCheck) columns() (outcome, boot *string, locked *bool, patch *int) {
	if kc.Outcome == "" {
		return nil, nil, nil, nil
	}
	o := kc.Outcome
	if kc.Result == nil {
		return &o, nil, nil, nil
	}
	b, l, p := kc.Result.VerifiedBoot, kc.Result.DeviceLocked, kc.Result.OSPatchLevel
	return &o, &b, &l, &p
}
