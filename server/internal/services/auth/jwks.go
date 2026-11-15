package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/time/rate"
)

// JWKSAuthService verifies asymmetric tokens (EdDSA/ES256/RS256) minted by
// the boson auth service, using its published JWKS. Keys are cached, refreshed
// periodically, and refetched when a token arrives with an unknown kid (key
// rotation).
type JWKSAuthService struct {
	keys     keyfunc.Keyfunc
	issuers  []string // any of these (empty = don't check)
	audience string
}

// NewJWKSAuthService starts the JWKS cache. jwksURLs and issuers are
// comma-separated lists: e.g. the auth service's JWKS and quicvoiced's
// video-ticket JWKS (keys are matched by kid). A JWKS endpoint that is down
// at startup isn't fatal: verification fails until it can be fetched.
func NewJWKSAuthService(ctx context.Context, jwksURLs, issuers, audience string) (*JWKSAuthService, error) {
	urls := splitList(jwksURLs)
	if len(urls) == 0 {
		return nil, errors.New("auth: JWKS URL is empty")
	}
	// keyfunc's defaults allow one unknown-kid refresh per 5 minutes and
	// fail a token that would wait over a minute for it: the second of two
	// tickets arriving together after a key change (or after the JWKS was
	// down at boot) was rejected, and a failed refresh locked everyone out
	// for 5 minutes. Allow a refresh every 200 ms instead. RateLimitWaitMax
	// bounds the wait for the limiter *and* the refresh request itself
	// (jwkset uses one context for both), so it must leave time for the
	// fetch. Verify also retries briefly, for keys another caller's refresh
	// is about to bring in.
	k, err := keyfunc.NewDefaultOverrideCtx(ctx, urls, keyfunc.Override{
		RefreshUnknownKID: rate.NewLimiter(rate.Every(200*time.Millisecond), 1),
		RateLimitWaitMax:  5 * time.Second,
		HTTPTimeout:       5 * time.Second,
		RefreshInterval:   15 * time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: JWKS %s: %w", jwksURLs, err)
	}
	return &JWKSAuthService{keys: k, issuers: splitList(issuers), audience: audience}, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// unknownKeyRetry: how long Verify keeps retrying a token whose key isn't
// in the JWKS yet (a refresh in flight or rate-limited), and how often.
var unknownKeyRetry, unknownKeyEvery = 3 * time.Second, 200 * time.Millisecond

func (s *JWKSAuthService) Verify(ctx context.Context, token string) (*Identity, error) {
	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"EdDSA", "ES256", "RS256"}),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30 * time.Second),
	}
	if s.audience != "" {
		opts = append(opts, jwt.WithAudience(s.audience))
	}
	var c Claims
	_, err := jwt.ParseWithClaims(token, &c, s.keys.Keyfunc, opts...)
	// ErrTokenUnverifiable = no key for its kid (yet). A signature, expiry or
	// audience failure isn't retried.
	for deadline := time.Now().Add(unknownKeyRetry); errors.Is(err, jwt.ErrTokenUnverifiable) && time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return nil, ErrUnauthenticated
		case <-time.After(unknownKeyEvery):
		}
		c = Claims{}
		_, err = jwt.ParseWithClaims(token, &c, s.keys.Keyfunc, opts...)
	}
	if err == nil && len(s.issuers) > 0 && !slices.Contains(s.issuers, c.Issuer) {
		return nil, ErrUnauthenticated
	}
	return identityFromClaims(&c, err)
}

// CompositeAuthService routes by the token's alg: HS256 to the shared-secret
// verifier, anything else to JWKS. Lets a deployment move from JWT_SECRET to
// the auth service without a flag day.
type CompositeAuthService struct {
	HMAC AuthServiceImpl // may be nil
	JWKS AuthServiceImpl // may be nil
}

func (s *CompositeAuthService) Verify(ctx context.Context, token string) (*Identity, error) {
	alg := tokenAlg(token)
	switch {
	case alg == "HS256" && s.HMAC != nil:
		return s.HMAC.Verify(ctx, token)
	case alg != "HS256" && alg != "" && s.JWKS != nil:
		return s.JWKS.Verify(ctx, token)
	}
	return nil, ErrUnauthenticated
}

// tokenAlg reads the alg header without verifying anything.
func tokenAlg(token string) string {
	if strings.Count(token, ".") != 2 {
		return ""
	}
	t, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
	if err != nil {
		return ""
	}
	alg, _ := t.Header["alg"].(string)
	return alg
}
