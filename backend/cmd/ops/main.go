// Command ops serves the operations console. Run it only on the company network (VPN or zero-trust
// proxy), never on the public internet; the API server doesn't serve these pages.
//
//	DATABASE_URL          PostgreSQL
//	ADDR                  listen address (default :8090)
//	OPS_BASE_URL          where staff open the console, e.g. https://ops.internal.gebeya.one
//	OIDC_ISSUER           the company identity provider
//	OIDC_CLIENT_ID        this console's client at the identity provider
//	OIDC_CLIENT_SECRET
//	OPS_REQUIRED_AMR      comma-separated authentication methods, one of which the sign-in must have used
//	                      (e.g. "hwk" for a hardware key). Required unless APP_ENV=dev.
//	OPS_SESSION_KEY       base64 of 32+ random bytes; signs the short-lived sign-in cookie
//	S3_*, DOCS_KEYS       KYB document storage, as for the API server (see kyb.FromEnv)
//	APP_ENV               "dev" allows OPS_REQUIRED_AMR to be empty (the dev identity provider has no MFA)
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/commerce"
	"superapp/backend/internal/kyb"
	"superapp/backend/internal/ledger"
	"superapp/backend/internal/merchants"
	"superapp/backend/internal/ops"
	"superapp/backend/internal/opsweb"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/payouts"
	"superapp/backend/internal/platform"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("ops console stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	need := func(k string) (string, error) {
		v := strings.TrimSpace(os.Getenv(k))
		if v == "" {
			return "", fmt.Errorf("%s is required", k)
		}
		return v, nil
	}
	var cfg opsweb.Config
	var err error
	dsn, err := need("DATABASE_URL")
	if err != nil {
		return err
	}
	for k, dst := range map[string]*string{"OPS_BASE_URL": &cfg.BaseURL, "OIDC_ISSUER": &cfg.Issuer,
		"OIDC_CLIENT_ID": &cfg.ClientID, "OIDC_CLIENT_SECRET": &cfg.ClientSecret} {
		if *dst, err = need(k); err != nil {
			return err
		}
	}
	key, err := need("OPS_SESSION_KEY")
	if err != nil {
		return err
	}
	if cfg.SessionKey, err = base64.StdEncoding.DecodeString(key); err != nil {
		return errors.New("OPS_SESSION_KEY must be base64")
	}
	for _, m := range strings.Split(os.Getenv("OPS_REQUIRED_AMR"), ",") {
		if m = strings.TrimSpace(m); m != "" {
			cfg.RequiredAMR = append(cfg.RequiredAMR, m)
		}
	}
	if len(cfg.RequiredAMR) == 0 {
		if os.Getenv("APP_ENV") != "dev" {
			return errors.New("OPS_REQUIRED_AMR is required: staff must sign in with a hardware key")
		}
		log.Warn("OPS_REQUIRED_AMR is empty: sign-in doesn't require MFA (dev only)")
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8090"
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	svc := &ops.Service{
		Pool:      pool,
		Merchants: &merchants.Service{Pool: pool},
		Payouts:   payouts.NewService(pool, ledger.New(), &platform.PINVerifier{Pool: pool}),
		// Staff only release or decline held payments; the rules and PIN check are the customer side's.
		Payments: payments.NewService(pool, ledger.New(), payments.DefaultRules(), &platform.PINVerifier{Pool: pool}),
	}
	commerce.New(pool, svc.Payments) // a released order payment moves its order on
	if svc.KYB, err = kyb.FromEnv(ctx, pool, os.Getenv); err != nil {
		return err
	}
	if svc.KYB == nil {
		log.Warn("document storage off (S3_ENDPOINT not set): approvals don't check KYB documents")
	}
	// The identity provider may start after us (docker compose); keep trying for a minute.
	var web *opsweb.Server
	for i := 0; ; i++ {
		if web, err = opsweb.New(ctx, svc, log, cfg); err == nil || i == 30 {
			break
		}
		log.Warn("waiting for the identity provider", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: addr, Handler: web.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	log.Info("ops console listening", "addr", addr, "base_url", cfg.BaseURL, "issuer", cfg.Issuer)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
