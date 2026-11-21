package notify

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// FCM sends through the Firebase Cloud Messaging HTTP v1 API, which reaches Android directly and iPhones
// through APNs once the Firebase project has the APNs key. It authenticates with a Google service
// account (the JSON key file downloaded from the Firebase console) and caches the access token.
type FCM struct {
	ProjectID   string
	ClientEmail string
	PrivateKey  *rsa.PrivateKey
	TokenURL    string // OAuth token endpoint from the key file
	SendURL     string // defaults to the FCM v1 endpoint for ProjectID
	HTTP        *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

const fcmScope = "https://www.googleapis.com/auth/firebase.messaging"

// NewFCM reads a service-account key file's contents.
func NewFCM(serviceAccountJSON []byte) (*FCM, error) {
	var key struct {
		Type        string `json:"type"`
		ProjectID   string `json:"project_id"`
		PrivateKey  string `json:"private_key"`
		ClientEmail string `json:"client_email"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal(serviceAccountJSON, &key); err != nil {
		return nil, fmt.Errorf("fcm: service account file: %w", err)
	}
	if key.Type != "service_account" || key.ProjectID == "" || key.ClientEmail == "" || key.PrivateKey == "" {
		return nil, errors.New("fcm: not a service account key file")
	}
	block, _ := pem.Decode([]byte(key.PrivateKey))
	if block == nil {
		return nil, errors.New("fcm: private key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("fcm: private key: %w", err)
	}
	rk, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("fcm: private key is not RSA")
	}
	if key.TokenURI == "" {
		key.TokenURI = "https://oauth2.googleapis.com/token"
	}
	return &FCM{ProjectID: key.ProjectID, ClientEmail: key.ClientEmail, PrivateKey: rk, TokenURL: key.TokenURI}, nil
}

func (f *FCM) client() *http.Client {
	if f.HTTP != nil {
		return f.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// accessToken returns a cached OAuth token, fetching a new one five minutes before the old one expires.
func (f *FCM) accessToken(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.token != "" && time.Now().Before(f.expires.Add(-5*time.Minute)) {
		return f.token, nil
	}
	now := time.Now()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signing := enc(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." + enc(map[string]any{
		"iss": f.ClientEmail, "scope": fcmScope, "aud": f.TokenURL, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.PrivateKey, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {signing + "." + base64.RawURLEncoding.EncodeToString(sig)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := f.client().Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return "", fmt.Errorf("fcm: token endpoint %d: %s", res.StatusCode, b)
	}
	if err := json.NewDecoder(res.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("fcm: token endpoint returned no token")
	}
	f.token, f.expires = tok.AccessToken, now.Add(time.Duration(tok.ExpiresIn)*time.Second)
	return f.token, nil
}

// Push sends one message. Tokens Firebase no longer knows come back as ErrTokenInvalid.
func (f *FCM) Push(ctx context.Context, token string, p Push) error {
	at, err := f.accessToken(ctx)
	if err != nil {
		return err
	}
	send := f.SendURL
	if send == "" {
		send = "https://fcm.googleapis.com/v1/projects/" + url.PathEscape(f.ProjectID) + "/messages:send"
	}
	data := p.Data
	if data == nil {
		data = map[string]string{}
	}
	body, _ := json.Marshal(map[string]any{"message": map[string]any{
		"token":        token,
		"notification": map[string]string{"title": p.Title, "body": p.Body},
		"data":         data,
		"android":      map[string]any{"priority": "HIGH"},
		"apns":         map[string]any{"payload": map[string]any{"aps": map[string]any{"sound": "default"}}},
	}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, send, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+at)
	req.Header.Set("Content-Type", "application/json")
	res, err := f.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	var e struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Details []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	for _, d := range e.Error.Details {
		if d.ErrorCode == "UNREGISTERED" || d.ErrorCode == "SENDER_ID_MISMATCH" {
			return fmt.Errorf("%w: %s", ErrTokenInvalid, d.ErrorCode)
		}
	}
	if res.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrTokenInvalid, e.Error.Status)
	}
	if res.StatusCode == http.StatusUnauthorized {
		f.mu.Lock()
		f.token = "" // fetch a fresh access token next time
		f.mu.Unlock()
	}
	return fmt.Errorf("fcm: send %d %s: %s", res.StatusCode, e.Error.Status, e.Error.Message)
}
