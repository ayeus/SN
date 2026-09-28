package httpx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ScopeFunc names the tenant an idempotency key belongs to, so two
// organisations can use the same key without colliding.
type ScopeFunc func(r *http.Request) string

// Idempotency replays the stored response for a repeated POST that carries the
// same Idempotency-Key (SRS IR-1). A retry after a timeout therefore cannot
// create a second deployment or a second API key.
//
// A key reused with a different body is rejected with 422, following the IETF
// idempotency-key draft. A request still in flight under the same key gets 409.
type Idempotency struct {
	pool  *pgxpool.Pool
	scope ScopeFunc
}

// NewIdempotency creates the middleware. scope must be safe to call after
// authentication has populated the request context.
func NewIdempotency(pool *pgxpool.Pool, scope ScopeFunc) *Idempotency {
	return &Idempotency{pool: pool, scope: scope}
}

// Wrap applies idempotency to POST requests that send an Idempotency-Key.
// Requests without the header pass straight through.
func (i *Idempotency) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if r.Method != http.MethodPost || key == "" || i.pool == nil {
			next.ServeHTTP(w, r)
			return
		}
		if len(key) > 255 {
			WriteProblem(w, http.StatusBadRequest, "Idempotency-Key must be at most 255 characters")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
		if err != nil {
			WriteProblem(w, http.StatusRequestEntityTooLarge, "Request body is too large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		sum := sha256.Sum256(append([]byte(r.URL.Path+"\n"), body...))
		reqHash := hex.EncodeToString(sum[:])

		scope := i.scope(r)
		if scope == "" {
			scope = "anon"
		}
		ctx := r.Context()

		// Claim the key. Exactly one concurrent request wins the insert.
		tag, err := i.pool.Exec(ctx, `
			INSERT INTO idempotency_keys (scope, key, method, path, request_hash)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (scope, key) DO NOTHING;
		`, scope, key, r.Method, r.URL.Path, reqHash)
		if err != nil {
			WriteProblem(w, http.StatusServiceUnavailable, "Unable to record idempotency key")
			return
		}

		if tag.RowsAffected() == 0 {
			i.replay(ctx, w, scope, key, reqHash)
			return
		}

		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// Only successful and client-error outcomes are final. A 5xx releases
		// the key so the client can retry the same operation.
		if rec.status >= 500 {
			_, _ = i.pool.Exec(context.WithoutCancel(ctx),
				`DELETE FROM idempotency_keys WHERE scope = $1 AND key = $2;`, scope, key)
			return
		}
		_, _ = i.pool.Exec(context.WithoutCancel(ctx), `
			UPDATE idempotency_keys SET status_code = $3, response_body = $4
			WHERE scope = $1 AND key = $2;
		`, scope, key, rec.status, rec.buf.Bytes())
	})
}

func (i *Idempotency) replay(ctx context.Context, w http.ResponseWriter, scope, key, reqHash string) {
	var storedHash string
	var status *int
	var body []byte
	err := i.pool.QueryRow(ctx, `
		SELECT request_hash, status_code, response_body
		FROM idempotency_keys WHERE scope = $1 AND key = $2;
	`, scope, key).Scan(&storedHash, &status, &body)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			WriteProblem(w, http.StatusConflict, "A request with this Idempotency-Key was just retried; try again")
			return
		}
		WriteProblem(w, http.StatusServiceUnavailable, "Unable to read idempotency key")
		return
	}
	if storedHash != reqHash {
		WriteProblem(w, http.StatusUnprocessableEntity, "Idempotency-Key was already used with a different request body")
		return
	}
	if status == nil {
		WriteProblem(w, http.StatusConflict, "A request with this Idempotency-Key is still being processed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Idempotent-Replayed", "true")
	w.WriteHeader(*status)
	_, _ = w.Write(body)
}

type recorder struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.buf.Write(b)
	return r.ResponseWriter.Write(b)
}
