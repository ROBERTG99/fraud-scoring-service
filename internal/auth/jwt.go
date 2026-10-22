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

// JWKS cache policy (security audit).
const (
	jwksFresh       = 10 * time.Minute // a known kid is served without a refetch
	jwksMinInterval = 10 * time.Second // at most one fetch attempt per interval, failed or not
	jwksMaxStale    = time.Hour        // keys this old are not used even when the issuer is down
)

// RemoteKeys fetches and caches a JWKS URL. One fetch runs at a time, outside the lock: concurrent callers
// with a usable cached key are served at once, the others wait for that fetch. Attempts are at most one per
// 10 s, also after a failure, and cached keys are accepted for at most an hour after the last good fetch.
type RemoteKeys struct {
	URL    string
	Client *http.Client
	Now    func() time.Time // nil: time.Now

	mu       sync.Mutex
	keys     StaticKeys
	fetched  time.Time     // last successful fetch
	tried    time.Time     // last attempt
	lastErr  error         // of the last attempt
	inflight chan struct{} // closed when the running fetch ends
}

func (r *RemoteKeys) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Key implements KeySource.
func (r *RemoteKeys) Key(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	for {
		r.mu.Lock()
		now := r.now()
		k, ok := r.keys[kid]
		age := now.Sub(r.fetched)
		usable := ok && age < jwksMaxStale
		if ok && age < jwksFresh {
			r.mu.Unlock()
			return k, nil
		}
		if wait := r.inflight; wait != nil {
			r.mu.Unlock()
			if usable {
				return k, nil
			}
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if !r.tried.IsZero() && now.Sub(r.tried) < jwksMinInterval {
			err := r.missingLocked(kid, ok)
			r.mu.Unlock()
			if usable {
				return k, nil
			}
			return nil, err
		}
		done := make(chan struct{})
		r.inflight, r.tried = done, now
		r.mu.Unlock()

		// Detached from the caller: its cancellation must not fail the fetch every waiter shares.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		keys, err := r.fetch(fctx)
		cancel()
		r.mu.Lock()
		if err == nil {
			r.keys, r.fetched = keys, r.now()
		}
		r.lastErr = err
		r.inflight = nil
		close(done)
		r.mu.Unlock()
		// Loop once more: the new keys, or (tried is now recent) the stale key or an error, without refetching.
	}
}

func (r *RemoteKeys) missingLocked(kid string, known bool) error {
	switch {
	case r.keys == nil || known:
		return fmt.Errorf("jwks unavailable: %v", r.lastErr)
	case r.lastErr != nil:
		return fmt.Errorf("unknown kid %q (jwks refresh failed: %v)", kid, r.lastErr)
	default:
		return fmt.Errorf("unknown kid %q", kid)
	}
}

func (r *RemoteKeys) fetch(ctx context.Context) (StaticKeys, error) {
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks: status %d", resp.StatusCode)
	}
	var doc JWKS
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&doc); err != nil {
		return nil, err
	}
	return KeysFromJWKS(doc)
}
