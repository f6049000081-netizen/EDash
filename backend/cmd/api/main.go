// Command api serves the auth, wallet and payments API.
//
//	DATABASE_URL          postgres connection string (required)
//	ADDR                  listen address (default :8080)
//	APP_ENV               "dev" is the only mode for now: SMS codes go to the log, device attestation is
//	                      simulated and sandbox top-ups are on. Any other value refuses to start until
//	                      the SMS aggregator and Play Integrity / App Attest verifiers are integrated.
//	AUTH_SIGNING_KEY      base64 32-byte Ed25519 seed for access tokens
//	OTP_HMAC_KEY          base64 key (32+ bytes) for hashing one-time codes
//	                      In dev, missing keys are generated at startup, so tokens stop working on restart.
//	OTP_ALLOWED_PREFIXES  comma-separated E.164 prefixes codes may be sent to (default: Ethiopia + diaspora list)
//	PUBLIC_BASE_URL       the API's URL as apps see it (e.g. https://api.example.et), for checking DPoP
//	                      proofs behind a proxy. Unset: taken from the request's Host.
//	DPOP_REQUIRED         platforms whose sign-ins and refreshes must bind the session to a device key
//	                      (DPoP): "android", "android,ios", or "true" for all. Unset: none.
//	KEY_ATTESTATION       Android key attestation of the DPoP and biometric keys: "off", "record"
//	                      (default: check and keep the outcome on the device) or "require" (refuse
//	                      Android sign-ins and fingerprint set-up without a key proven to be in secure
//	                      hardware; implies DPoP on Android). Revocations come from Google's status list.
//	ANDROID_PACKAGE       application ID the keys must belong to (default one.gebeya.gebeya_one)
//	ANDROID_SIGNER_SHA256 comma-separated SHA-256 (hex) of the app signing certificates allowed. Unset:
//	                      not checked (dev builds are signed with a local key).
//	OTP_RESEND_SECONDS    minimum gap between sign-in codes to one number (default 30)
//	OTP_HOURLY_MAX        sign-in codes per number per hour (default 5)
//	CLIENT_IP_HEADER      header a trusted proxy sets with the client address, e.g. CF-Connecting-IP
//	KAFKA_BROKERS         comma-separated brokers. Set: the outbox relay publishes to Kafka and the
//	                      notifications service consumes from it (group "notify"). Unset: events go
//	                      straight from the outbox to the notifications service in this process.
//	FCM_SERVICE_ACCOUNT_FILE  Firebase service-account key (JSON). Set: notifications are also pushed to
//	                      phones. Unset: they are kept for the in-app list only.
//
// telebirr top-ups are enabled only when the merchant credentials are set (see telebirrConfig).
// Without them the telebirr rail answers rail_unavailable. Nothing is simulated.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/bizweb"
	"superapp/backend/internal/commerce"
	"superapp/backend/internal/httpapi"
	"superapp/backend/internal/identity"
	"superapp/backend/internal/keyattest"
	"superapp/backend/internal/kyb"
	"superapp/backend/internal/ledger"
	"superapp/backend/internal/merchants"
	"superapp/backend/internal/notify"
	"superapp/backend/internal/outbox"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/payouts"
	"superapp/backend/internal/platform"
	"superapp/backend/internal/rails/telebirr"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	if os.Getenv("APP_ENV") != "dev" {
		return errors.New("only APP_ENV=dev is supported until the SMS aggregator and device attestation are integrated")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	signingSeed, err := secret(log, "AUTH_SIGNING_KEY", 32)
	if err != nil {
		return err
	}
	otpKey, err := secret(log, "OTP_HMAC_KEY", 32)
	if err != nil {
		return err
	}
	tokens, err := identity.NewTokenSigner(signingSeed)
	if err != nil {
		return err
	}
	cfg := identity.DefaultConfig()
	cfg.PublicBaseURL = os.Getenv("PUBLIC_BASE_URL")
	switch v := os.Getenv("DPOP_REQUIRED"); v {
	case "", "false":
	case "true":
		cfg.DPoPRequired = []string{"android", "ios", "web"}
	default:
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p != "android" && p != "ios" && p != "web" {
				return fmt.Errorf("DPOP_REQUIRED: unknown platform %q", p)
			}
			cfg.DPoPRequired = append(cfg.DPoPRequired, p)
		}
	}
	cfg.KeyAttestation = os.Getenv("KEY_ATTESTATION")
	switch cfg.KeyAttestation {
	case "":
		cfg.KeyAttestation = identity.AttestRecord
	case identity.AttestOff, identity.AttestRecord, identity.AttestRequire:
	default:
		return fmt.Errorf("KEY_ATTESTATION: want off, record or require, got %q", cfg.KeyAttestation)
	}
	attester := &keyattest.Verifier{Roots: keyattest.GoogleRoots(), Status: &keyattest.StatusList{},
		Package: "one.gebeya.gebeya_one"}
	if v := os.Getenv("ANDROID_PACKAGE"); v != "" {
		attester.Package = v
	}
	for _, h := range strings.Split(os.Getenv("ANDROID_SIGNER_SHA256"), ",") {
		if h = strings.ReplaceAll(strings.TrimSpace(h), ":", ""); h == "" {
			continue
		}
		d, err := hex.DecodeString(h)
		if err != nil || len(d) != 32 {
			return fmt.Errorf("ANDROID_SIGNER_SHA256: %q isn't a SHA-256 in hex", h)
		}
		attester.SignerDigests = append(attester.SignerDigests, d)
	}
	if v := os.Getenv("OTP_ALLOWED_PREFIXES"); v != "" {
		cfg.AllowedPrefixes = strings.Split(v, ",")
	}
	// Sign-in code throttles. The defaults (30 s between codes, 5 per number per hour) are the fraud
	// controls; the dev server loosens them in backend/.env so testers aren't locked out.
	if v := os.Getenv("OTP_RESEND_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("OTP_RESEND_SECONDS: want a whole number of seconds, got %q", v)
		}
		cfg.OTPResendAfter = time.Duration(n) * time.Second
	}
	if v := os.Getenv("OTP_HOURLY_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("OTP_HOURLY_MAX: want a positive number, got %q", v)
		}
		cfg.OTPHourlyMax = n
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	ids, err := identity.NewService(pool, tokens, otpKey, identity.DevSMS{Log: log}, identity.DevAttestation{}, cfg, log)
	if err != nil {
		return err
	}
	ids.KeyAttester = attester
	log.Info("device keys", "dpop_required", cfg.DPoPRequired, "key_attestation", cfg.KeyAttestation,
		"signer_checked", len(attester.SignerDigests) > 0)
	l := ledger.New()
	svc := payments.NewService(pool, l, payments.DefaultRules(), &platform.PINVerifier{Pool: pool})
	svc.Biometric = &platform.BiometricVerifier{Pool: pool}
	webhooks := map[string]httpapi.NotificationVerifier{}
	tb, err := telebirrClient()
	if err != nil {
		return err
	}
	if tb != nil {
		svc.Rails["telebirr"] = tb
		webhooks["telebirr"] = tb
		go sweepTopUps(ctx, log, svc)
		log.Info("telebirr top-ups enabled")
	} else {
		log.Info("telebirr top-ups disabled: merchant credentials not configured")
	}
	// Merchant payouts. No payout rail is connected until a licensed partner's credentials exist, so
	// batches are created and wait as "scheduled" with rail_unavailable.
	pay := payouts.NewService(pool, l, &platform.PINVerifier{Pool: pool})
	pay.Log = log
	go pay.Run(ctx, 5*time.Minute)
	log.Info("merchant payouts: batching on; no payout rail configured, so nothing is sent")

	// Shops and orders. Orders are paid into escrow; unpaid and unanswered orders are cancelled (and
	// refunded) by the sweep.
	shops := commerce.New(pool, svc)
	go shops.Run(ctx, time.Minute, log)

	notifier := &notify.Service{Pool: pool, Log: log}
	if f := os.Getenv("FCM_SERVICE_ACCOUNT_FILE"); f != "" {
		key, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("FCM_SERVICE_ACCOUNT_FILE: %w", err)
		}
		fcm, err := notify.NewFCM(key)
		if err != nil {
			return err
		}
		notifier.Pusher = fcm
		log.Info("push notifications enabled", "firebase_project", fcm.ProjectID)
	} else {
		log.Info("push notifications disabled: FCM_SERVICE_ACCOUNT_FILE not set; notifications show in the app only")
	}
	relay := &outbox.Relay{Pool: pool, Log: log}
	if b := os.Getenv("KAFKA_BROKERS"); b != "" {
		brokers := strings.Split(b, ",")
		producer, err := outbox.NewKafkaClient(brokers)
		if err != nil {
			return err
		}
		defer producer.Close()
		consumer, err := outbox.NewKafkaConsumer(brokers, "notify", notify.Topics)
		if err != nil {
			return err
		}
		defer consumer.Close()
		relay.Publisher = outbox.Kafka{Client: producer}
		go func() { _ = outbox.Consume(ctx, consumer, notifier, log) }()
		log.Info("outbox relay publishing to Kafka", "brokers", brokers)
	} else {
		relay.Publisher = outbox.Direct{Handlers: []outbox.Handler{notifier}}
		log.Info("outbox relay delivering in-process (KAFKA_BROKERS not set)")
	}
	go relay.Run(ctx)

	docs, err := kyb.FromEnv(ctx, pool, os.Getenv)
	if err != nil {
		return err
	}
	if docs == nil {
		log.Warn("document storage off (S3_ENDPOINT not set): merchants can't send KYB documents")
	}
	if pics, err := kyb.S3FromEnv(ctx, os.Getenv, "S3_PHOTOS_BUCKET", "product-photos"); err != nil {
		return err
	} else if pics != nil {
		shops.Photos = pics
	} else {
		log.Warn("product photos off (S3_ENDPOINT not set)")
	}

	srv := &http.Server{
		Addr: addr,
		Handler: (&httpapi.Server{
			Pool: pool, Ledger: l, Payments: svc, Identity: ids, Auth: ids, Log: log, Notify: notifier, Payouts: pay,
			Merchants: &merchants.Service{Pool: pool}, KYB: docs, Commerce: shops,
			Web:      &bizweb.Store{Pool: pool, PIN: &platform.PINVerifier{Pool: pool}},
			Webhooks: webhooks, Sandbox: true, ClientIPHeader: os.Getenv("CLIENT_IP_HEADER"),
		}).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr, "env", "dev", "token_kid", identity.KeyID(tokens.PublicKey()))
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

// secret reads a base64 key from the environment. In dev a missing key is generated (and lost on restart).
func secret(log *slog.Logger, name string, minLen int) ([]byte, error) {
	v := os.Getenv(name)
	if v == "" {
		b := make([]byte, minLen)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		log.Warn("generated an ephemeral key; set it to keep sessions across restarts", "var", name)
		return b, nil
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("%s: not valid base64: %w", name, err)
	}
	if len(b) < minLen {
		return nil, fmt.Errorf("%s: need at least %d bytes, got %d", name, minLen, len(b))
	}
	if name == "AUTH_SIGNING_KEY" && len(b) != 32 {
		return nil, fmt.Errorf("%s: must be exactly 32 bytes", name)
	}
	return b, nil
}

// telebirrClient builds the telebirr rail from TELEBIRR_* variables, or returns nil if none are set.
//
//	TELEBIRR_BASE_URL, TELEBIRR_CHECKOUT_URL        API gateway and web checkout URLs from the merchant portal
//	TELEBIRR_FABRIC_APP_ID, TELEBIRR_APP_SECRET     fabric credentials
//	TELEBIRR_MERCHANT_APP_ID, TELEBIRR_MERCHANT_CODE
//	TELEBIRR_PRIVATE_KEY                            our RSA private key (PEM or base64 DER)
//	TELEBIRR_PUBLIC_KEY                             telebirr's RSA public key, for notifications
//	TELEBIRR_NOTIFY_URL                             public https URL of /v1/webhooks/rails/telebirr
//	TELEBIRR_REDIRECT_URL                           optional return URL after checkout
//	TELEBIRR_SIGN_PADDING (pss|pkcs1), TELEBIRR_TRADE_TYPE, TELEBIRR_PAYEE_TYPE, TELEBIRR_BUSINESS_TYPE
//	                                                optional; see internal/rails/telebirr
func telebirrClient() (*telebirr.Client, error) {
	env := func(k string) string { return strings.TrimSpace(os.Getenv("TELEBIRR_" + k)) }
	if env("MERCHANT_APP_ID") == "" && env("FABRIC_APP_ID") == "" && env("PRIVATE_KEY") == "" {
		return nil, nil
	}
	cfg := telebirr.Config{
		BaseURL: env("BASE_URL"), CheckoutURL: env("CHECKOUT_URL"),
		FabricAppID: env("FABRIC_APP_ID"), AppSecret: env("APP_SECRET"),
		MerchantAppID: env("MERCHANT_APP_ID"), MerchantCode: env("MERCHANT_CODE"),
		NotifyURL: env("NOTIFY_URL"), RedirectURL: env("REDIRECT_URL"),
		Padding: telebirr.Padding(env("SIGN_PADDING")), TradeType: env("TRADE_TYPE"),
		PayeeType: env("PAYEE_TYPE"), BusinessType: env("BUSINESS_TYPE"),
	}
	var err error
	if v := env("PRIVATE_KEY"); v != "" {
		if cfg.PrivateKey, err = telebirr.ParsePrivateKey(v); err != nil {
			return nil, fmt.Errorf("TELEBIRR_PRIVATE_KEY: %w", err)
		}
	}
	if v := env("PUBLIC_KEY"); v != "" {
		if cfg.PublicKey, err = telebirr.ParsePublicKey(v); err != nil {
			return nil, fmt.Errorf("TELEBIRR_PUBLIC_KEY: %w", err)
		}
	}
	return telebirr.New(cfg)
}

// sweepTopUps settles top-ups whose notification never arrived, once a minute.
func sweepTopUps(ctx context.Context, log *slog.Logger, svc *payments.Service) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := svc.SyncPendingTopUps(ctx, 2*time.Minute, 100)
			if err != nil {
				log.Error("top-up sweep", "err", err)
			}
			if n > 0 {
				log.Info("top-up sweep settled", "count", n)
			}
		}
	}
}
