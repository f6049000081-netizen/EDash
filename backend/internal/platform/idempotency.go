package platform

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxBody = 1 << 20

// Idempotent makes a state-changing endpoint safe to retry. Clients send an Idempotency-Key (UUID).
//   - First request with a key: runs the handler and stores the response.
//   - Same key, same body: replays the stored response without running the handler again.
//   - Same key, different body: 409 idempotency_conflict.
//   - Same key while the first request is still running: 409, retry shortly.
//
// Server errors (5xx) are not stored, so the client can retry them with the same key.
// Must run after the auth middleware: keys are scoped to the caller.
func Idempotent(pool *pgxpool.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if _, err := uuid.Parse(key); err != nil {
			WriteProblem(w, NewProblem(http.StatusBadRequest, "validation_failed", "Missing Idempotency-Key",
				"Send a UUID in the Idempotency-Key header with every request that changes state."))
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			WriteProblem(w, NewProblem(http.StatusBadRequest, "validation_failed", "Unreadable body", ""))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		principal := "user:" + UserFrom(r.Context()).String()
		sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), body...))
		reqHash := hex.EncodeToString(sum[:])
		ctx := r.Context()

		tag, err := pool.Exec(ctx, `
			INSERT INTO platform.idempotency_keys (key, principal, request_hash)
			VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, key, principal, reqHash)
		if err != nil {
			WriteProblem(w, NewProblem(http.StatusInternalServerError, "internal", "Server error", ""))
			return
		}
		if tag.RowsAffected() == 0 {
			var storedHash string
			var status *int16
			var stored []byte
			if err := pool.QueryRow(ctx, `
				SELECT request_hash, response_status, response_body FROM platform.idempotency_keys
				WHERE principal = $1 AND key = $2`, principal, key).Scan(&storedHash, &status, &stored); err != nil {
				WriteProblem(w, NewProblem(http.StatusInternalServerError, "internal", "Server error", ""))
				return
			}
			switch {
			case storedHash != reqHash:
				WriteProblem(w, NewProblem(http.StatusConflict, "idempotency_conflict", "Key reused",
					"This Idempotency-Key was already used for a different request."))
			case status == nil:
				WriteProblem(w, NewProblem(http.StatusConflict, "idempotency_conflict", "Request in progress",
					"The first request with this key is still running. Retry in a moment."))
			default:
				if *status != http.StatusNoContent {
					ct := "application/json"
					if *status >= 400 {
						ct = "application/problem+json"
					}
					w.Header().Set("Content-Type", ct)
				}
				w.Header().Set("Idempotent-Replayed", "true")
				w.WriteHeader(int(*status))
				_, _ = w.Write(stored)
			}
			return
		}

		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		storable := rec.status == http.StatusNoContent || jsonish(rec.body.Bytes())
		if rec.status >= 500 || !storable {
			_, _ = pool.Exec(ctx, `DELETE FROM platform.idempotency_keys WHERE principal = $1 AND key = $2`, principal, key)
			return
		}
		_, _ = pool.Exec(ctx, `
			UPDATE platform.idempotency_keys SET response_status = $3, response_body = $4
			WHERE principal = $1 AND key = $2`, principal, key, rec.status, nilIfEmpty(rec.body.Bytes()))
	})
}

func nilIfEmpty(b []byte) []byte {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	return b
}

func jsonish(b []byte) bool {
	b = bytes.TrimSpace(b)
	return len(b) > 0 && (b[0] == '{' || b[0] == '[')
}

type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(s int) { r.status = s; r.ResponseWriter.WriteHeader(s) }
func (r *recorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}
