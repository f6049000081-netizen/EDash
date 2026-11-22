// Command business serves the web business console, where shops handle orders, products, their
// storefront and money on a computer. Sign-in is approved from the app (see internal/bizweb).
//
//	DATABASE_URL        PostgreSQL
//	ADDR                listen address (default :8070)
//	BUSINESS_BASE_URL   where people open the console, e.g. https://business.gebeya.one. Must be https in
//	                    production: the session cookies are Secure (browsers allow http://localhost only).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/bizweb"
	"superapp/backend/internal/commerce"
	"superapp/backend/internal/kyb"
	"superapp/backend/internal/ledger"
	"superapp/backend/internal/merchants"
	"superapp/backend/internal/payments"
	"superapp/backend/internal/payouts"
	"superapp/backend/internal/platform"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("business console stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	base := strings.TrimSpace(os.Getenv("BUSINESS_BASE_URL"))
	if dsn == "" || base == "" {
		return errors.New("DATABASE_URL and BUSINESS_BASE_URL are required")
	}
	if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://localhost") {
		return errors.New("BUSINESS_BASE_URL must be https (or http://localhost for development)")
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8070"
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	pin := &platform.PINVerifier{Pool: pool}
	// Shops cancel orders (refunds) and hand them over (settlement) here, through the same services.
	pay := payments.NewService(pool, ledger.New(), payments.DefaultRules(), pin)
	shops := commerce.New(pool, pay)
	if pics, err := kyb.S3FromEnv(ctx, os.Getenv, "S3_PHOTOS_BUCKET", "product-photos"); err != nil {
		return err
	} else if pics != nil {
		shops.Photos = pics
	}
	web, err := bizweb.New(base, &bizweb.Store{Pool: pool, PIN: pin}, &merchants.Service{Pool: pool}, shops,
		payouts.NewService(pool, ledger.New(), pin), log)
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
	log.Info("business console listening", "addr", addr, "base_url", base)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
