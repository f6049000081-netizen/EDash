// Command devtool does operations-staff jobs on a local development database, until the operations
// console (with maker-checker approval) exists. It refuses to run against anything but a local database.
//
//	DATABASE_URL=postgres://...@g1db:5432/g1dev devtool approve-merchant <merchant id | TIN>
package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/merchants"
)

// devApprover is recorded as approved_by, so dev approvals are easy to find and never mistaken for staff.
var devApprover = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://gebeya.one/dev/devtool"))

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "devtool:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	u, err := url.Parse(dsn)
	if err != nil || dsn == "" {
		return fmt.Errorf("set DATABASE_URL")
	}
	if h := u.Hostname(); h != "g1db" && h != "localhost" && h != "127.0.0.1" {
		return fmt.Errorf("refusing to run against %s: devtool is for local development databases only", h)
	}
	if len(args) != 2 || args[0] != "approve-merchant" {
		return fmt.Errorf("usage: devtool approve-merchant <merchant id | TIN>")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	id, err := uuid.Parse(args[1])
	if err != nil {
		if err := pool.QueryRow(ctx, `SELECT id FROM merchant.merchants WHERE tin = $1`, strings.TrimSpace(args[1])).Scan(&id); err != nil {
			return fmt.Errorf("no merchant with id or TIN %q", args[1])
		}
	}
	m, err := (&merchants.Service{Pool: pool}).Approve(ctx, id, devApprover)
	if err != nil {
		return err
	}
	fmt.Printf("approved %s (%s), merchant number %s\n", m.TradeName, m.ID, m.MerchantNo)
	return nil
}
