// Package telebirr talks to Ethio Telecom's telebirr merchant ("fabric") API to collect wallet
// top-ups: apply a fabric token, create an order (preOrder), send the customer to telebirr's checkout,
// receive the signed payment notification, and confirm the result with queryOrder.
//
// No official specification was available when this was written. Field names, the signing rules
// and the status values follow public open-source integrations (see README). Every point where those
// sources disagree is configurable. Before going live, check this file and client.go against the
// specification and test keys that come with the telebirr merchant agreement.
package telebirr

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Padding selects the RSA signature scheme. telebirr labels requests "SHA256WithRSA" but the public
// integrations sign with RSA-PSS (SHA-256, MGF1-SHA-256, 32-byte salt); PKCS #1 v1.5 is kept as an
// option because one integration makes it configurable.
type Padding string

const (
	PaddingPSS   Padding = "pss"
	PaddingPKCS1 Padding = "pkcs1"
)

const signType = "SHA256WithRSA"

// Keys never part of the signed string. biz_content is not signed as a blob: its fields are
// flattened into the same map instead.
var unsignedKeys = map[string]bool{
	"sign": true, "sign_type": true, "header": true, "refund_info": true,
	"openType": true, "raw_request": true, "biz_content": true,
}

// canonical builds "k1=v1&k2=v2..." over the top-level fields plus the fields of biz_content,
// sorted by key in byte order, without URL-encoding. A biz_content field overrides a top-level
// field of the same name.
func canonical(top map[string]string, biz map[string]string) string {
	all := make(map[string]string, len(top)+len(biz))
	for k, v := range top {
		if !unsignedKeys[k] {
			all[k] = v
		}
	}
	for k, v := range biz {
		if !unsignedKeys[k] {
			all[k] = v
		}
	}
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(all[k])
	}
	return b.String()
}

var (
	pssOpts = &rsa.PSSOptions{SaltLength: 32, Hash: crypto.SHA256}
	// Verification accepts any salt length, in case telebirr signs with a different one than we do.
	pssVerifyOpts = &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthAuto, Hash: crypto.SHA256}
)

func sign(key *rsa.PrivateKey, pad Padding, text string) (string, error) {
	sum := sha256.Sum256([]byte(text))
	var sig []byte
	var err error
	if pad == PaddingPKCS1 {
		sig, err = rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	} else {
		sig, err = rsa.SignPSS(rand.Reader, key, crypto.SHA256, sum[:], pssOpts)
	}
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

var errBadSignature = errors.New("telebirr: signature does not verify")

func verify(pub *rsa.PublicKey, pad Padding, text, signature string) error {
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return errBadSignature
	}
	sum := sha256.Sum256([]byte(text))
	if pad == PaddingPKCS1 {
		err = rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig)
	} else {
		err = rsa.VerifyPSS(pub, crypto.SHA256, sum[:], sig, pssVerifyOpts)
	}
	if err != nil {
		return errBadSignature
	}
	return nil
}

// derBytes accepts a PEM block or bare base64 DER, the two forms telebirr's portal hands out.
func derBytes(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if block, _ := pem.Decode([]byte(s)); block != nil {
		return block.Bytes, nil
	}
	b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		return nil, errors.New("key is neither PEM nor base64 DER")
	}
	return b, nil
}

// ParsePrivateKey reads an RSA private key in PKCS #8 or PKCS #1 form.
func ParsePrivateKey(s string) (*rsa.PrivateKey, error) {
	der, err := derBytes(s)
	if err != nil {
		return nil, err
	}
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, errors.New("private key is not RSA")
	}
	k, err := x509.ParsePKCS1PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	return k, nil
}

// ParsePublicKey reads an RSA public key in PKIX (SubjectPublicKeyInfo) or PKCS #1 form.
func ParsePublicKey(s string) (*rsa.PublicKey, error) {
	der, err := derBytes(s)
	if err != nil {
		return nil, err
	}
	if k, err := x509.ParsePKIXPublicKey(der); err == nil {
		if rk, ok := k.(*rsa.PublicKey); ok {
			return rk, nil
		}
		return nil, errors.New("public key is not RSA")
	}
	k, err := x509.ParsePKCS1PublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	return k, nil
}

// SignFields signs top-level fields plus biz_content fields the way requests are signed.
func SignFields(key *rsa.PrivateKey, pad Padding, top, biz map[string]string) (string, error) {
	return sign(key, pad, canonical(top, biz))
}

// VerifyFields checks a signature made by SignFields.
func VerifyFields(pub *rsa.PublicKey, pad Padding, top, biz map[string]string, signature string) error {
	return verify(pub, pad, canonical(top, biz), signature)
}
