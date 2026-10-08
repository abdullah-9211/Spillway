package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type Role string

const (
	RoleAdmin  Role = "admin"
	RoleViewer Role = "viewer"
)

func (r Role) Valid() bool { return r == RoleAdmin || r == RoleViewer }

// Claims are what a session token says. The Go service issues and verifies them, so the role never comes
// from the web app's say-so.
type Claims struct {
	UserID   string `json:"sub"`
	Username string `json:"usr"`
	Role     Role   `json:"role"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
}

const SessionLifetime = 12 * time.Hour

var (
	ErrInvalidToken = errors.New("invalid session token")
	ErrExpiredToken = errors.New("session token expired")
)

// Signer issues and checks HMAC-SHA256 tokens of the form base64url(claims) "." base64url(mac).
type Signer struct {
	secret []byte
	now    func() time.Time
}

// MinSecretLen is the shortest signing secret accepted.
const MinSecretLen = 32

func NewSigner(secret []byte) (*Signer, error) { return NewSignerWithClock(secret, time.Now) }

// NewSignerWithClock is NewSigner with an injectable clock, for tests of expiry.
func NewSignerWithClock(secret []byte, now func() time.Time) (*Signer, error) {
	if len(secret) < MinSecretLen {
		return nil, errors.New("ADMIN_SESSION_SECRET must be at least 32 bytes")
	}
	return &Signer{secret: secret, now: now}, nil
}

func (s *Signer) mac(payload string) []byte {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

func (s *Signer) Issue(c Claims) (token string, expires time.Time, err error) {
	now := s.now()
	c.IssuedAt, c.Expires = now.Unix(), now.Add(SessionLifetime).Unix()
	raw, err := json.Marshal(c)
	if err != nil {
		return "", time.Time{}, err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.mac(payload)), time.Unix(c.Expires, 0).UTC(), nil
}

// Verify returns the claims of a token that is correctly signed, unexpired, and carries a known role.
func (s *Signer) Verify(token string) (Claims, error) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok || payload == "" || sig == "" {
		return Claims{}, ErrInvalidToken
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(payload)) { // constant time
		return Claims{}, ErrInvalidToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil || !c.Role.Valid() || c.Username == "" {
		return Claims{}, ErrInvalidToken
	}
	if s.now().Unix() >= c.Expires {
		return Claims{}, ErrExpiredToken
	}
	return c, nil
}
