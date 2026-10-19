// Package firebaseauth verifies Firebase Auth ID tokens (RS256 JWTs) against
// Google's public x509 certificates, using only the project id — no service
// account is ever needed or read. Checks follow Firebase's "verify ID tokens
// using a third-party JWT library" rules.
package firebaseauth

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// DefaultCertsURL is Google's securetoken signing-cert endpoint.
const DefaultCertsURL = "https://www.googleapis.com/robot/v1/metadata/x509/securetoken@system.gserviceaccount.com"

const (
	clockSkew     = 5 * time.Minute
	defaultMaxAge = time.Hour
	maxSubLen     = 128
)

// Claims is the verified identity.
type Claims struct {
	UID   string
	Email string
}

// Verifier turns a bearer ID token into Claims. The sync handlers depend on
// this interface so tests can inject identities without minting JWTs.
type Verifier interface {
	Verify(ctx context.Context, idToken string) (Claims, error)
}

// ErrInvalidToken wraps every verification failure (never shown to clients).
var ErrInvalidToken = errors.New("invalid firebase id token")

// Google verifies tokens for one Firebase project.
type Google struct {
	projectID string
	certsURL  string
	client    *http.Client
	now       func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	expires time.Time
}

// New builds a verifier. certsURL is overridable only for tests / dev builds;
// pass "" to use DefaultCertsURL.
func New(projectID, certsURL string) *Google {
	if certsURL == "" {
		certsURL = DefaultCertsURL
	}
	return &Google{
		projectID: projectID,
		certsURL:  certsURL,
		client:    &http.Client{Timeout: 10 * time.Second},
		now:       time.Now,
	}
}

type fbClaims struct {
	Email    string           `json:"email"`
	AuthTime *jwt.NumericDate `json:"auth_time"`
	jwt.RegisteredClaims
}

// Verify validates signature, iss, aud, exp, iat, auth_time and sub.
func (g *Google) Verify(ctx context.Context, idToken string) (Claims, error) {
	var c fbClaims
	now := g.now()
	tok, err := jwt.ParseWithClaims(idToken, &c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("missing kid")
		}
		return g.key(ctx, kid)
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithTimeFunc(g.now),
		jwt.WithLeeway(clockSkew),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer("https://securetoken.google.com/"+g.projectID),
		jwt.WithAudience(g.projectID),
	)
	if err != nil || !tok.Valid {
		return Claims{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if c.IssuedAt == nil || c.IssuedAt.After(now.Add(clockSkew)) {
		return Claims{}, fmt.Errorf("%w: iat missing or in the future", ErrInvalidToken)
	}
	if c.AuthTime == nil || c.AuthTime.After(now.Add(clockSkew)) {
		return Claims{}, fmt.Errorf("%w: auth_time missing or in the future", ErrInvalidToken)
	}
	if c.Subject == "" || len(c.Subject) > maxSubLen {
		return Claims{}, fmt.Errorf("%w: bad sub", ErrInvalidToken)
	}
	return Claims{UID: c.Subject, Email: c.Email}, nil
}

var maxAgeRe = regexp.MustCompile(`max-age=(\d+)`)

// key returns the public key for kid, refreshing the cached cert set when it
// has expired (Cache-Control max-age) or the kid is unknown (key rotation).
func (g *Google) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if k, ok := g.keys[kid]; ok && g.now().Before(g.expires) {
		return k, nil
	}
	if err := g.refreshLocked(ctx); err != nil {
		if k, ok := g.keys[kid]; ok { // serve stale on fetch failure
			return k, nil
		}
		return nil, err
	}
	k, ok := g.keys[kid]
	if !ok {
		return nil, errors.New("unknown kid")
	}
	return k, nil
}

func (g *Google) refreshLocked(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.certsURL, nil)
	if err != nil {
		return err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cert fetch status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var pems map[string]string
	if err := json.Unmarshal(body, &pems); err != nil {
		return err
	}
	keys := make(map[string]*rsa.PublicKey, len(pems))
	for kid, p := range pems {
		block, _ := pem.Decode([]byte(p))
		if block == nil {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		if pk, ok := cert.PublicKey.(*rsa.PublicKey); ok {
			keys[kid] = pk
		}
	}
	if len(keys) == 0 {
		return errors.New("no usable certs")
	}
	age := defaultMaxAge
	if m := maxAgeRe.FindStringSubmatch(resp.Header.Get("Cache-Control")); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			age = time.Duration(n) * time.Second
		}
	}
	g.keys = keys
	g.expires = g.now().Add(age)
	return nil
}

type ctxKey struct{}

// WithClaims stores verified claims in ctx.
func WithClaims(ctx context.Context, c Claims) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// FromContext returns the claims set by Middleware.
func FromContext(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(ctxKey{}).(Claims)
	return c, ok
}

// Middleware requires "Authorization: Bearer <ID token>" and puts the uid and
// email in the request context; anything else is 401.
func Middleware(v Verifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, tok, found := strings.Cut(r.Header.Get("Authorization"), " ")
		if !found || !strings.EqualFold(scheme, "Bearer") || tok == "" {
			unauthorized(w)
			return
		}
		claims, err := v.Verify(r.Context(), tok)
		if err != nil {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithClaims(r.Context(), claims)))
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
}
