package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidServiceToken is returned when a service-to-service credential fails
// verification.
var ErrInvalidServiceToken = errors.New("auth: invalid service token")

// Internal service identities. These name the caller in a signed header so that
// the billing meter can distinguish "the inference gateway reported usage" from
// "someone on the network posted a usage event".
const (
	ServiceInferenceGateway = "inference-gateway"
	ServiceRouter           = "router"
	ServiceControlAPI       = "control-api"
	ServiceScheduler        = "scheduler"
	ServiceCoordinator      = "coordinator"
	ServiceBillingMeter     = "billing-meter"
	ServiceTrustEngine      = "trust-engine"
)

// internalAuthHeader carries the signed service credential.
const internalAuthHeader = "X-AyeusANN-Service-Auth"

// serviceTokenSkew bounds how far a service token's timestamp may drift. It caps
// the replay window for a captured header.
const serviceTokenSkew = 5 * time.Minute

// ServiceAuthenticator issues and verifies short-lived HMAC credentials for
// internal service-to-service calls. Internal APIs (usage ingestion, routing)
// must never be reachable with only network access.
type ServiceAuthenticator struct {
	secret  []byte
	service string
}

// NewServiceAuthenticator creates an authenticator for the named service.
// The shared secret must be at least 32 bytes and identical across services.
func NewServiceAuthenticator(secret, service string) (*ServiceAuthenticator, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("auth: internal service secret must be at least 32 bytes, got %d", len(secret))
	}
	if service == "" {
		return nil, errors.New("auth: service name is required")
	}
	return &ServiceAuthenticator{secret: []byte(secret), service: service}, nil
}

// Sign produces a credential identifying this service. The signature covers the
// service name and issue time, so the header cannot be edited to impersonate a
// different service or replayed indefinitely.
func (a *ServiceAuthenticator) Sign() string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	payload := a.service + ":" + ts
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(payload))
	return payload + ":" + hex.EncodeToString(mac.Sum(nil))
}

// SignRequest attaches the service credential to an outgoing request.
func (a *ServiceAuthenticator) SignRequest(r *http.Request) {
	r.Header.Set(internalAuthHeader, a.Sign())
}

// Verify checks a credential and returns the calling service's name.
func (a *ServiceAuthenticator) Verify(credential string) (string, error) {
	parts := strings.Split(credential, ":")
	if len(parts) != 3 {
		return "", ErrInvalidServiceToken
	}
	service, ts, sig := parts[0], parts[1], parts[2]

	expected := hmac.New(sha256.New, a.secret)
	expected.Write([]byte(service + ":" + ts))
	expectedSig := hex.EncodeToString(expected.Sum(nil))

	if subtle.ConstantTimeCompare([]byte(sig), []byte(expectedSig)) != 1 {
		return "", ErrInvalidServiceToken
	}

	issued, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", ErrInvalidServiceToken
	}
	age := time.Since(time.Unix(issued, 0))
	if age > serviceTokenSkew || age < -serviceTokenSkew {
		return "", fmt.Errorf("%w: credential timestamp outside acceptable window", ErrInvalidServiceToken)
	}

	return service, nil
}

// RequireInternalService returns middleware that admits only requests signed by
// one of the named services.
func (a *ServiceAuthenticator) RequireInternalService(allowed ...string) func(http.Handler) http.Handler {
	permitted := make(map[string]bool, len(allowed))
	for _, s := range allowed {
		permitted[s] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			credential := r.Header.Get(internalAuthHeader)
			if credential == "" {
				writeJSONError(w, http.StatusUnauthorized, "Internal service credential required")
				return
			}

			caller, err := a.Verify(credential)
			if err != nil {
				writeJSONError(w, http.StatusUnauthorized, "Invalid internal service credential")
				return
			}

			if len(permitted) > 0 && !permitted[caller] {
				writeJSONError(w, http.StatusForbidden, "Service is not permitted to call this endpoint")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
