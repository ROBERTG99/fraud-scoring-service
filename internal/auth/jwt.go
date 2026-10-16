package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Audience of console tokens.
const Audience = "fraud-console"

// Claims carried by dev-issuer tokens.
type Claims struct {
	Role string `json:"role"`
	Name string `json:"name,omitempty"`
	jwt.RegisteredClaims
}

// JWK is one Ed25519 public key (RFC 8037).
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

// JWKS is a key set document.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// Signer mints tokens with an Ed25519 key (dev-issuer only).
type Signer struct {
	Issuer string
	Kid    string
	key    ed25519.PrivateKey
	now    func() time.Time
}

// NewSigner generates a fresh key pair; keys live only in the dev-issuer process.
func NewSigner(issuer string) (*Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	kid := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)[:8])
	return &Signer{Issuer: issuer, Kid: kid, key: priv, now: time.Now}, nil
}

// JWKS returns the public key set.
func (s *Signer) JWKS() JWKS {
	pub := s.key.Public().(ed25519.PublicKey)
	return JWKS{Keys: []JWK{{Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(pub), Kid: s.Kid, Alg: "EdDSA", Use: "sig"}}}
}

// Mint returns a signed token for subject with role, valid for ttl.
func (s *Signer) Mint(subject, role, name string, ttl time.Duration) (string, error) {
	now := s.now()
	claims := Claims{Role: role, Name: name, RegisteredClaims: jwt.RegisteredClaims{
		Issuer: s.Issuer, Subject: subject, Audience: jwt.ClaimStrings{Audience},
		IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now.Add(-5 * time.Second)), ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = s.Kid
	return tok.SignedString(s.key)
}

// Identity is a verified caller.
type Identity struct {
	Subject string
	Role    string
	Name    string
}

// KeySource returns the public key for a kid.
type KeySource interface {
	Key(ctx context.Context, kid string) (ed25519.PublicKey, error)
}

// Verifier validates console tokens: EdDSA only, issuer, audience, expiry and a known role.
type Verifier struct {
	Issuer string
	Keys   KeySource
}

// ErrInvalidToken wraps every verification failure.
var ErrInvalidToken = errors.New("invalid token")

// Verify parses and validates a token.
func (v *Verifier) Verify(ctx context.Context, token string) (Identity, error) {
	var claims Claims
	parsed, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.Keys.Key(ctx, kid)
	}, jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}), jwt.WithIssuer(v.Issuer), jwt.WithAudience(Audience),
		jwt.WithExpirationRequired(), jwt.WithLeeway(5*time.Second))
	if err != nil || !parsed.Valid {
		return Identity{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if !slices.Contains(ConsoleRoles, claims.Role) || claims.Subject == "" {
		return Identity{}, fmt.Errorf("%w: unknown role %q", ErrInvalidToken, claims.Role)
	}
	return Identity{Subject: claims.Subject, Role: claims.Role, Name: claims.Name}, nil
}

// StaticKeys is a fixed key set (tests, or a pinned JWKS).
type StaticKeys map[string]ed25519.PublicKey

// Key implements KeySource.
func (s StaticKeys) Key(_ context.Context, kid string) (ed25519.PublicKey, error) {
	if k, ok := s[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown kid %q", kid)
}

// KeysFromJWKS decodes Ed25519 keys from a JWKS document.
func KeysFromJWKS(doc JWKS) (StaticKeys, error) {
	out := StaticKeys{}
	for _, k := range doc.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("bad key %s", k.Kid)
		}
		out[k.Kid] = ed25519.PublicKey(raw)
	}
	return out, nil
}

// RemoteKeys fetches and caches a JWKS URL; an unknown kid triggers at most one refresh per 10 s.
type RemoteKeys struct {
	URL    string
	Client *http.Client
	mu     sync.Mutex
	keys   StaticKeys
	last   time.Time
}

// Key implements KeySource.
func (r *RemoteKeys) Key(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if k, ok := r.keys[kid]; ok && time.Since(r.last) < 10*time.Minute {
		return k, nil
	}
	if time.Since(r.last) > 10*time.Second || r.keys == nil {
		if err := r.refresh(ctx); err != nil && r.keys == nil {
			return nil, err
		}
	}
	if k, ok := r.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown kid %q", kid)
}

func (r *RemoteKeys) refresh(ctx context.Context) error {
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks: status %d", resp.StatusCode)
	}
	var doc JWKS
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&doc); err != nil {
		return err
	}
	keys, err := KeysFromJWKS(doc)
	if err != nil {
		return err
	}
	r.keys, r.last = keys, time.Now()
	return nil
}
