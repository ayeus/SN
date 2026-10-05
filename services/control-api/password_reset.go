package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/smtp"
	"strings"
	"sync"
	"time"

	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/httpx"
	"github.com/ayeus/ayeusann/internal/platform"
	"github.com/jackc/pgx/v5"
)

// resetTokenTTL bounds how long an emailed reset link works.
const resetTokenTTL = time.Hour

// Mailer delivers transactional email.
type Mailer interface {
	Send(to, subject, body string) error
}

// smtpMailer sends through an SMTP relay (SMTP_HOST, SMTP_PORT, SMTP_USER,
// SMTP_PASSWORD, SMTP_FROM).
type smtpMailer struct{ host, port, user, pass, from string }

func (m smtpMailer) Send(to, subject, body string) error {
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		m.from, to, subject, body)
	var a smtp.Auth
	if m.user != "" {
		a = smtp.PlainAuth("", m.user, m.pass, m.host)
	}
	return smtp.SendMail(m.host+":"+m.port, a, m.from, []string{to}, []byte(msg))
}

// logMailer is the development mailer: with no SMTP relay configured it writes
// the message to the service log so a developer can follow the link. It is
// never used in production.
type logMailer struct{ api *API }

func (m logMailer) Send(to, subject, body string) error {
	m.api.log.Info("DEV MAIL (no SMTP configured; not sent)", "to", to, "subject", subject, "body", body)
	return nil
}

// NewMailer returns the configured mailer, or nil when email cannot be sent.
func NewMailer(a *API) Mailer {
	if host := platform.Env("SMTP_HOST", ""); host != "" {
		return smtpMailer{
			host: host, port: platform.Env("SMTP_PORT", "587"),
			user: platform.Env("SMTP_USER", ""), pass: platform.Env("SMTP_PASSWORD", ""),
			from: platform.Env("SMTP_FROM", "no-reply@localhost"),
		}
	}
	if !platform.IsProduction() {
		return logMailer{api: a}
	}
	return nil
}

// attemptLimiter is a per-process sliding-window limiter for credential
// endpoints. It bounds online password guessing and reset-email flooding.
type attemptLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	max    int
	window time.Duration
}

func newAttemptLimiter(max int, window time.Duration) *attemptLimiter {
	return &attemptLimiter{hits: map[string][]time.Time{}, max: max, window: window}
}

// allow records an attempt and reports whether it is within the limit.
func (l *attemptLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	if len(l.hits) > 50_000 { // bound memory under a spray of distinct keys
		for k, v := range l.hits {
			if len(v) == 0 || now.Sub(v[len(v)-1]) > l.window {
				delete(l.hits, k)
			}
		}
	}
	return true
}

// blocked reports whether key has used up its attempts, without recording one.
func (l *attemptLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	n := 0
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.window {
			n++
		}
	}
	return n >= l.max
}

// fail records a failed attempt. Unlike allow it always records, so attempts
// made while blocked keep the block in place.
func (l *attemptLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) > l.max { // enough to stay blocked; no need to grow without bound
		kept = kept[len(kept)-l.max:]
	}
	l.hits[key] = append(kept, now)
	if len(l.hits) > 50_000 {
		for k, v := range l.hits {
			if len(v) == 0 || now.Sub(v[len(v)-1]) > l.window {
				delete(l.hits, k)
			}
		}
	}
}

// reset forgets key's attempts, after a success.
func (l *attemptLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// HandleForgotPassword emails a single-use reset link. The response is the same
// whether or not the address has an account, so it cannot be used to discover
// which emails are registered.
func (a *API) HandleForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if httpx.DecodeJSON(w, r, &req) != nil {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !validEmail(email) {
		httpx.WriteProblemFields(w, http.StatusBadRequest, "Enter a valid email address", map[string]string{"email": "Enter a valid email address"})
		return
	}
	if a.mailer == nil {
		writeError(w, http.StatusServiceUnavailable, "This installation cannot send email. Ask the person who runs it for a reset link.")
		return
	}
	ip := "unknown"
	if p := clientIP(r); p != nil {
		ip = *p
	}
	if !a.resetLimiter.allow(ip) || !a.resetLimiter.allow("email:"+email) {
		writeError(w, http.StatusTooManyRequests, "Too many reset requests. Try again in an hour.")
		return
	}

	ctx := r.Context()
	var userID string
	err := a.db.Pool.QueryRow(ctx, `SELECT id FROM users WHERE email = $1 AND deleted_at IS NULL AND disabled_at IS NULL AND password_hash IS NOT NULL;`, email).Scan(&userID)
	if err == nil {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			writeError(w, http.StatusInternalServerError, "Could not create a reset link")
			return
		}
		token := hex.EncodeToString(raw)
		if _, err := a.db.Pool.Exec(ctx, `
			INSERT INTO auth_tokens (user_id, kind, token_hash, expires_at)
			VALUES ($1, 'password_reset', $2, NOW() + ($3 * INTERVAL '1 second'));
		`, userID, hashToken(token), int(resetTokenTTL.Seconds())); err != nil {
			writeError(w, http.StatusInternalServerError, "Could not create a reset link")
			return
		}
		link := a.publicBase(r) + "/reset?token=" + token
		body := "Someone asked to reset the password for this account.\n\nSet a new password (link valid for 1 hour):\n" + link +
			"\n\nIf this wasn't you, ignore this email; your password is unchanged."
		if err := a.mailer.Send(email, "Reset your password", body); err != nil {
			a.log.Error("reset email failed", "err", err)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "Database error")
		return
	}

	_, devLog := a.mailer.(logMailer)
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "sent_if_registered",
		// True only on development installs with no SMTP relay, so the console
		// can tell the developer where the link went instead of implying an
		// email was delivered.
		"delivered_to_log": devLog,
	})
}

// HandleResetPassword sets a new password from a reset link and signs out every
// existing session.
func (a *API) HandleResetPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if httpx.DecodeJSON(w, r, &req) != nil {
		return
	}
	if err := auth.ValidatePassword(req.Password); err != nil {
		httpx.WriteProblemFields(w, http.StatusBadRequest, "Choose a stronger password",
			map[string]string{"password": strings.TrimPrefix(err.Error(), "auth: ")})
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	var userID string
	err = a.db.ExecTx(ctx, func(tx pgx.Tx) error {
		// Consuming the token and changing the password are one transaction, so
		// a link works exactly once even under concurrent requests.
		if err := tx.QueryRow(ctx, `
			UPDATE auth_tokens SET consumed_at = NOW()
			WHERE token_hash = $1 AND kind = 'password_reset' AND consumed_at IS NULL AND expires_at > NOW()
			RETURNING user_id;
		`, hashToken(strings.TrimSpace(req.Token))).Scan(&userID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, updated_at = NOW() WHERE id = $1;`, userID, hash)
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusBadRequest, "This reset link is invalid or has expired. Request a new one.")
			return
		}
		writeError(w, http.StatusInternalServerError, "Could not reset the password")
		return
	}
	if err := a.revocations.RevokeAllForUser(ctx, userID, "password reset"); err != nil {
		a.log.Error("failed to revoke sessions after password reset", "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "password_updated"})
}
