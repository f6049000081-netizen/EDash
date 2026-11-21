package platform

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Biometric confirmation: the app keeps a P-256 key in the phone's secure hardware that can only be used
// after the user passes the phone's fingerprint or face check. Confirming a payment with it means
// signing the payment's confirm message (see payments.ConfirmMessage). The server holds only the public
// key, registered with the PIN (identity.EnrollBiometric).

var (
	ErrBiometricNotEnrolled = errors.New("biometric confirmation isn't set up on this device")
	ErrBiometricInvalid     = errors.New("biometric signature didn't verify")
)

// ParseBiometricKey accepts a P-256 public key as base64 DER SubjectPublicKeyInfo (what Android
// Keystore exports) or as a base64 uncompressed point (what the iOS Secure Enclave exports), and
// returns it as base64 SPKI for storage.
func ParseBiometricKey(b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		if raw, err = base64.RawURLEncoding.DecodeString(b64); err != nil {
			return "", errors.New("the key isn't base64")
		}
	}
	var pub *ecdsa.PublicKey
	if len(raw) == 65 && raw[0] == 4 {
		if _, err := ecdh.P256().NewPublicKey(raw); err != nil {
			return "", errors.New("the key isn't a P-256 point")
		}
		pub = &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(raw[1:33]), Y: new(big.Int).SetBytes(raw[33:])}
	} else {
		k, err := x509.ParsePKIXPublicKey(raw)
		if err != nil {
			return "", errors.New("the key isn't a public key")
		}
		ec, ok := k.(*ecdsa.PublicKey)
		if !ok || ec.Curve != elliptic.P256() {
			return "", errors.New("the key must be ECDSA P-256")
		}
		pub = ec
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// BiometricVerifier checks a signature from the calling device's biometric key.
type BiometricVerifier struct {
	Pool *pgxpool.Pool
}

// Verify checks sig (ASN.1 DER ECDSA, base64) over msg with the key registered for this user's device.
func (v *BiometricVerifier) Verify(ctx context.Context, userID, deviceID uuid.UUID, msg []byte, sigB64 string) error {
	var key *string
	err := v.Pool.QueryRow(ctx, `
		SELECT biometric_key_pub FROM identity.devices WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		deviceID, userID).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && key == nil) {
		return ErrBiometricNotEnrolled
	}
	if err != nil {
		return err
	}
	der, err := base64.StdEncoding.DecodeString(*key)
	if err != nil {
		return err
	}
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return err
	}
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok {
		return ErrBiometricInvalid
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		if sig, err = base64.RawURLEncoding.DecodeString(sigB64); err != nil {
			return ErrBiometricInvalid
		}
	}
	sum := sha256.Sum256(msg)
	if !ecdsa.VerifyASN1(pub, sum[:], sig) {
		return ErrBiometricInvalid
	}
	return nil
}
