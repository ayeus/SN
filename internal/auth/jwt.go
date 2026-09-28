package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var (
	ErrInvalidToken   = errors.New("auth: invalid or expired JWT token")
	ErrInvalidClaims  = errors.New("auth: invalid token claims")
	ErrWrongTokenType = errors.New("auth: token is not valid for this purpose")
	ErrSecretTooShort = errors.New("auth: JWT secret must be at least 32 bytes")
)

// Token audiences. A token minted for one purpose must not be accepted for
// another: a refresh token cannot call an API route, and an access token cannot
// register a host.
const (
	AudienceAPI          = "ayeusann-api"
	AudienceRefresh      = "ayeusann-refresh"
	AudienceRegistration = "ayeusann-host-registration"
)

// RoleHostInstaller is the role carried by host registration tokens. It grants
// no API access; it exists solely to register a host with the coordinator.
const RoleHostInstaller = "host_installer"

// Claims defines the custom claims stored in AyeusANN JWTs.
type Claims struct {
	UserID string `json:"uid"`
	OrgID  string `json:"org_id,omitempty"`
	Role   string `json:"role,omitempty"`
	// Tier is set only on host registration tokens: the supply tier the owner
	// declared when generating the install command (PRD F-10).
	Tier string `json:"tier,omitempty"`
	jwt.RegisteredClaims
}

// TokenID returns the token's unique identifier (jti), used for revocation.
func (c *Claims) TokenID() string { return c.ID }

// HasAudience reports whether the token was minted for the given audience.
func (c *Claims) HasAudience(want string) bool {
	for _, aud := range c.Audience {
		if aud == want {
			return true
		}
	}
	return false
}

// TokenManager handles JWT issuance and verification.
type TokenManager struct {
	secret        []byte
	issuer        string
	accessExpiry  time.Duration
	refreshExpiry time.Duration
	regExpiry     time.Duration
}

// NewTokenManager creates a TokenManager. It returns an error when the secret is
// too short to be a safe HMAC key, so a service cannot start with a weak signer.
func NewTokenManager(secret string, accessExpiry, refreshExpiry time.Duration) (*TokenManager, error) {
	if len(secret) < 32 {
		return nil, ErrSecretTooShort
	}
	if accessExpiry == 0 {
		accessExpiry = 15 * time.Minute
	}
	if refreshExpiry == 0 {
		refreshExpiry = 7 * 24 * time.Hour
	}
	return &TokenManager{
		secret:        []byte(secret),
		issuer:        "ayeusann",
		accessExpiry:  accessExpiry,
		refreshExpiry: refreshExpiry,
		regExpiry:     24 * time.Hour,
	}, nil
}

// AccessExpiry returns the configured access token lifetime.
func (tm *TokenManager) AccessExpiry() time.Duration { return tm.accessExpiry }

// RefreshExpiry returns the configured refresh token lifetime.
func (tm *TokenManager) RefreshExpiry() time.Duration { return tm.refreshExpiry }

// RegistrationExpiry returns the host registration token lifetime.
func (tm *TokenManager) RegistrationExpiry() time.Duration { return tm.regExpiry }

// TokenPair is the result of issuing credentials for a session.
type TokenPair struct {
	AccessToken      string
	AccessJTI        string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshJTI       string
	RefreshExpiresAt time.Time
}

// GenerateTokenPair issues an access token and a refresh token. Both carry a
// unique jti so they can be individually revoked, and distinct audiences so
// neither can be substituted for the other.
func (tm *TokenManager) GenerateTokenPair(userID, orgID, role string) (*TokenPair, error) {
	now := time.Now()

	accessJTI := uuid.NewString()
	accessExpiresAt := now.Add(tm.accessExpiry)
	accessToken, err := tm.sign(Claims{
		UserID: userID,
		OrgID:  orgID,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        accessJTI,
			Issuer:    tm.issuer,
			Subject:   userID,
			Audience:  jwt.ClaimStrings{AudienceAPI},
			ExpiresAt: jwt.NewNumericDate(accessExpiresAt),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("auth: failed to sign access token: %w", err)
	}

	refreshJTI := uuid.NewString()
	refreshExpiresAt := now.Add(tm.refreshExpiry)
	refreshToken, err := tm.sign(Claims{
		UserID: userID,
		OrgID:  orgID,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        refreshJTI,
			Issuer:    tm.issuer,
			Subject:   userID,
			Audience:  jwt.ClaimStrings{AudienceRefresh},
			ExpiresAt: jwt.NewNumericDate(refreshExpiresAt),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("auth: failed to sign refresh token: %w", err)
	}

	return &TokenPair{
		AccessToken:      accessToken,
		AccessJTI:        accessJTI,
		AccessExpiresAt:  accessExpiresAt,
		RefreshToken:     refreshToken,
		RefreshJTI:       refreshJTI,
		RefreshExpiresAt: refreshExpiresAt,
	}, nil
}

// RegistrationToken is a single-use credential for enrolling a host machine.
type RegistrationToken struct {
	Token     string
	JTI       string
	ExpiresAt time.Time
}

// GenerateRegistrationToken issues a host registration token valid for 24 hours.
// The token's audience restricts it to the coordinator's registration path, and
// its jti is recorded by the caller so the token can be consumed exactly once.
// tier is the supply tier the host will enrol at ("t1", "t2" or "t3").
func (tm *TokenManager) GenerateRegistrationToken(userID, orgID, tier string) (*RegistrationToken, error) {
	now := time.Now()
	jti := uuid.NewString()
	expiresAt := now.Add(tm.regExpiry)

	token, err := tm.sign(Claims{
		UserID: userID,
		OrgID:  orgID,
		Role:   RoleHostInstaller,
		Tier:   tier,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			Issuer:    tm.issuer,
			Subject:   userID,
			Audience:  jwt.ClaimStrings{AudienceRegistration},
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("auth: failed to sign registration token: %w", err)
	}

	return &RegistrationToken{Token: token, JTI: jti, ExpiresAt: expiresAt}, nil
}

func (tm *TokenManager) sign(claims Claims) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(tm.secret)
}

// VerifyToken parses and validates a token, requiring the given audience.
// Callers must pass the audience appropriate to the operation being performed;
// this is what prevents a refresh or registration token from acting as an
// API credential.
func (tm *TokenManager) VerifyToken(tokenString, requiredAudience string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("auth: unexpected signing method: %v", t.Header["alg"])
		}
		return tm.secret, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(tm.issuer),
		jwt.WithAudience(requiredAudience),
		jwt.WithExpirationRequired(),
	)

	if err != nil || !token.Valid {
		return nil, ErrInvalidToken
	}

	claims, ok := token.Claims.(*Claims)
	if !ok {
		return nil, ErrInvalidClaims
	}
	if claims.ID == "" {
		// Every token this version issues carries a jti; a token without one
		// predates revocation support and is no longer trusted.
		return nil, ErrInvalidClaims
	}
	if !claims.HasAudience(requiredAudience) {
		return nil, ErrWrongTokenType
	}

	return claims, nil
}

// VerifyAccessToken validates a token for use as an API credential.
func (tm *TokenManager) VerifyAccessToken(tokenString string) (*Claims, error) {
	return tm.VerifyToken(tokenString, AudienceAPI)
}

// VerifyRefreshToken validates a token for use at the refresh endpoint.
func (tm *TokenManager) VerifyRefreshToken(tokenString string) (*Claims, error) {
	return tm.VerifyToken(tokenString, AudienceRefresh)
}

// VerifyRegistrationToken validates a host enrolment token.
func (tm *TokenManager) VerifyRegistrationToken(tokenString string) (*Claims, error) {
	claims, err := tm.VerifyToken(tokenString, AudienceRegistration)
	if err != nil {
		return nil, err
	}
	if claims.Role != RoleHostInstaller {
		return nil, ErrWrongTokenType
	}
	return claims, nil
}
