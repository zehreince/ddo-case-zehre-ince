package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const issuer = "carsihub"

// Tokens access token üretir ve doğrular (HS256).
type Tokens struct {
	secret    []byte
	accessTTL time.Duration
	now       func() time.Time
}

func NewTokens(secret string, accessTTL time.Duration) *Tokens {
	return &Tokens{secret: []byte(secret), accessTTL: accessTTL, now: time.Now}
}

type claims struct {
	Role Role `json:"role"`
	jwt.RegisteredClaims
}

func (t *Tokens) IssueAccess(p Principal) (string, time.Time, error) {
	now := t.now()
	exp := now.Add(t.accessTTL)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		Role: p.Role,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   p.UserID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	})
	s, err := tok.SignedString(t.secret)
	return s, exp, err
}

// ParseAccess imzayı, algoritmayı (yalnızca HS256; "none" reddedilir),
// yayıncıyı ve süreyi doğrular.
func (t *Tokens) ParseAccess(raw string) (Principal, error) {
	var c claims
	_, err := jwt.ParseWithClaims(raw, &c,
		func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(t.now),
	)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if c.Subject == "" || !c.Role.Valid() {
		return Principal{}, ErrInvalidToken
	}
	return Principal{UserID: c.Subject, Role: c.Role}, nil
}

// NewRefreshToken opak, tahmin edilemez bir token ve veritabanında saklanacak özetini üretir.
func NewRefreshToken() (raw string, hash []byte, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	raw = base64.RawURLEncoding.EncodeToString(b[:])
	return raw, HashRefreshToken(raw), nil
}

func HashRefreshToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}
