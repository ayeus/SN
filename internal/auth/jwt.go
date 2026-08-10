package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrInvalidToken = errors.New("auth: invalid or expired JWT token")
	ErrInvalidClaims = errors.New("auth: invalid token claims")
)

// Claims defines the custom claims stored in AyeusANN JWTs.
type Claims struct {
	UserID string `json:"uid"`
	OrgID  string `json:"org_id,omitempty"`
	Role   string `json:"role,omitempty"`
	jwt.RegisteredClaims
}

// TokenManager handles JWT issuance and verification.
type TokenManager struct {
	secret        []byte
	issuer        string
	accessExpiry  time.Duration
	refreshExpiry time.Duration
}

// NewTokenManager creates a TokenManager.
func NewTokenManager(secret string, accessExpiry, refreshExpiry time.Duration) *TokenManager {
	if accessExpiry == 0 {
		accessExpiry = 15 * time.Minute
	}
	if refreshExpiry == 0 {
		refreshExpiry = 7 * 24 * time.Hour
	}
	return &TokenManager{
		secret:        []byte(secret),
		issuer:        "AyeusANN",
		accessExpiry:  accessExpiry,
		refreshExpiry: refreshExpiry,
	}
}

// GeneratePair generates an access token and a refresh token for a user and organization context.
func (tm *TokenManager) GeneratePair(userID, orgID, role string) (accessToken string, refreshToken string, err error) {
	now := time.Now()

	// Access Token Claims
	accessClaims := Claims{
		UserID: userID,
		OrgID:  orgID,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tm.issuer,
			Subject:   userID,
			Audience:  jwt.ClaimStrings{"AyeusANN-api"},
			ExpiresAt: jwt.NewNumericDate(now.Add(tm.accessExpiry)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims)
	accessToken, err = token.SignedString(tm.secret)
	if err != nil {
		return "", "", fmt.Errorf("auth: failed to sign access token: %w", err)
	}

	// Refresh Token Claims
	refreshClaims := Claims{
		UserID: userID,
		OrgID:  orgID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tm.issuer,
			Subject:   userID,
			Audience:  jwt.ClaimStrings{"AyeusANN-refresh"},
			ExpiresAt: jwt.NewNumericDate(now.Add(tm.refreshExpiry)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}

	rToken := jwt.NewWithClaims(jwt.SigningMethodHS256, refreshClaims)
	refreshToken, err = rToken.SignedString(tm.secret)
	if err != nil {
		return "", "", fmt.Errorf("auth: failed to sign refresh token: %w", err)
	}

	return accessToken, refreshToken, nil
}

// VerifyToken parses and validates a JWT token string, returning its Claims.
func (tm *TokenManager) VerifyToken(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("auth: unexpected signing method: %v", t.Header["alg"])
		}
		return tm.secret, nil
	})

	if err != nil || !token.Valid {
		return nil, ErrInvalidToken
	}

	claims, ok := token.Claims.(*Claims)
	if !ok {
		return nil, ErrInvalidClaims
	}

	return claims, nil
}
