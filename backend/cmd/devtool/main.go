// Command devtool does setup jobs on a local development database: approving a merchant without the
// console, and inviting staff (in production, staff management needs a second person). It refuses to
// run against anything but a local database.
//
//	DATABASE_URL=postgres://...@g1db:5432/g1dev devtool approve-merchant <merchant id | TIN>
//	DATABASE_URL=... devtool invite-staff <email> <full name> <role>[,<role>...]
package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"superapp/backend/internal/merchants"
	"superapp/backend/internal/ops"
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
	if len(args) == 1 && args[0] == "hash-password" { // for the dev identity provider's config
		pw, err := io.ReadAll(io.LimitReader(os.Stdin, 200))
		if err != nil {
			return err
		}
		h, err := bcrypt.GenerateFromPassword([]byte(strings.TrimSpace(string(pw))), 12)
		if err != nil {
			return err
		}
		fmt.Println(string(h))
		return nil
	}
	dsn := os.Getenv("DATABASE_URL")
	u, err := url.Parse(dsn)
	if err != nil || dsn == "" {
		return fmt.Errorf("set DATABASE_URL")
	}
	if h := u.Hostname(); h != "g1db" && h != "localhost" && h != "127.0.0.1" {
		return fmt.Errorf("refusing to run against %s: devtool is for local development databases only", h)
	}
	usage := fmt.Errorf("usage: devtool approve-merchant <merchant id | TIN>\n       devtool invite-staff <email> <full name> <role>[,<role>...]")
	if len(args) == 0 {
		return usage
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	switch {
	case args[0] == "invite-staff" && len(args) == 4:
		svc := &ops.Service{Pool: pool}
		id, err := svc.Invite(ctx, args[1], args[2], strings.Split(args[3], ","), ops.Bootstrap)
		if err != nil {
			return err
		}
		fmt.Printf("invited %s (%s) as %s; they bind their account on first sign-in\n", args[2], id, args[3])
		return nil
	case args[0] == "approve-merchant" && len(args) == 2:
	default:
		return usage
	}
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
