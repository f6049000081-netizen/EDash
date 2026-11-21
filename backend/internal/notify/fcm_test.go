package notify_test

import (
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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"superapp/backend/internal/notify"
)

// A stand-in for Google's token endpoint and the FCM send endpoint, used only by this test.
func TestFCM(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))

	var tokenCalls, sends atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenCalls.Add(1)
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
				http.Error(w, "grant", 400)
				return
			}
			parts := strings.Split(r.Form.Get("assertion"), ".")
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
				http.Error(w, "signature", 401)
				return
			}
			raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var claims map[string]any
			_ = json.Unmarshal(raw, &claims)
			if claims["iss"] != "push@g1-test.iam.gserviceaccount.com" || claims["aud"] != srv.URL+"/token" ||
				claims["scope"] != "https://www.googleapis.com/auth/firebase.messaging" {
				http.Error(w, "claims", 401)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-1", "expires_in": 3600})
		case "/v1/projects/g1-test/messages:send":
			sends.Add(1)
			if r.Header.Get("Authorization") != "Bearer at-1" {
				http.Error(w, "auth", 401)
				return
			}
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Message struct {
					Token        string            `json:"token"`
					Notification map[string]string `json:"notification"`
					Data         map[string]string `json:"data"`
				} `json:"message"`
			}
			_ = json.Unmarshal(body, &req)
			if req.Message.Token == "gone" {
				w.WriteHeader(404)
				_, _ = w.Write([]byte(`{"error":{"code":404,"status":"NOT_FOUND","message":"Requested entity was not found.",
					"details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNREGISTERED"}]}}`))
				return
			}
			if req.Message.Notification["title"] != "Money received" || req.Message.Data["intent_id"] != "i-1" {
				http.Error(w, "payload", 400)
				return
			}
			_, _ = w.Write([]byte(`{"name":"projects/g1-test/messages/1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	file, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "g1-test", "private_key": pemKey,
		"client_email": "push@g1-test.iam.gserviceaccount.com", "token_uri": srv.URL + "/token",
	})
	f, err := notify.NewFCM(file)
	if err != nil {
		t.Fatal(err)
	}
	f.SendURL = srv.URL + "/v1/projects/g1-test/messages:send"
	p := notify.Push{Title: "Money received", Body: "Abebe sent you ETB 150.00.", Data: map[string]string{"intent_id": "i-1"}}
	for i := 0; i < 2; i++ {
		if err := f.Push(context.Background(), "tok-1", p); err != nil {
			t.Fatal(err)
		}
	}
	if tokenCalls.Load() != 1 || sends.Load() != 2 {
		t.Errorf("token fetched %d times for %d sends; it should be cached", tokenCalls.Load(), sends.Load())
	}
	if err := f.Push(context.Background(), "gone", p); !errors.Is(err, notify.ErrTokenInvalid) {
		t.Errorf("unregistered token: %v", err)
	}

	for name, bad := range map[string]string{
		"not json":        "{",
		"user credential": `{"type":"authorized_user"}`,
		"bad key":         `{"type":"service_account","project_id":"p","client_email":"e","private_key":"nope"}`,
	} {
		if _, err := notify.NewFCM([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
