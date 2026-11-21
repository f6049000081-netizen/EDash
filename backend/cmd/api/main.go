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

	"superapp/backend/internal/httpapi"
	"superapp/backend/internal/identity"
	"superapp/backend/internal/ledger"
	"superapp/backend/internal/notify"
	"superapp/backend/internal/outbox"
	"superapp/backend/internal/payments"
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
	l := ledger.New()
	svc := payments.NewService(pool, l, payments.DefaultRules(), &platform.PINVerifier{Pool: pool})
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

	srv := &http.Server{
		Addr: addr,
		Handler: (&httpapi.Server{
			Pool: pool, Ledger: l, Payments: svc, Identity: ids, Auth: ids, Log: log, Notify: notifier,
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
