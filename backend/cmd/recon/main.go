// Command recon imports a partner's daily statement and prints the reconciliation report as JSON.
//
//	DATABASE_URL=... recon -rail telebirr -date 2026-11-20 -file statement.csv
//
// The file must be in the normalised CSV format described in internal/recon. The exit status is 0
// when everything matched, 2 when there are exceptions to resolve, and 1 on errors.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"superapp/backend/internal/recon"
)

func main() {
	rail := flag.String("rail", "telebirr", "rail the statement is from")
	date := flag.String("date", "", "business date, YYYY-MM-DD (Ethiopian time)")
	file := flag.String("file", "", "statement file in the normalised CSV format")
	flag.Parse()
	code, err := run(*rail, *date, *file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "recon:", err)
	}
	os.Exit(code)
}

func run(rail, date, file string) (int, error) {
	if date == "" || file == "" {
		return 1, fmt.Errorf("-date and -file are required")
	}
	day, err := time.ParseInLocation("2006-01-02", date, recon.EAT)
	if err != nil {
		return 1, fmt.Errorf("-date: %w", err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return 1, err
	}
	lines, err := recon.ParseCSV(bytes.NewReader(raw))
	if err != nil {
		return 1, err
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return 1, fmt.Errorf("DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return 1, err
	}
	defer pool.Close()

	sum := sha256.Sum256(raw)
	rep, err := recon.Import(ctx, pool, rail, day, hex.EncodeToString(sum[:]), lines)
	if err != nil {
		return 1, err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return 1, err
	}
	if !rep.Clean() {
		return 2, nil
	}
	return 0, nil
}
