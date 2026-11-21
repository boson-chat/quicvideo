package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/boson-chat/quicvideo/server/internal/jwks"
	"github.com/boson-chat/quicvideo/server/internal/logger"
	"github.com/golang-jwt/jwt/v5"
)

// JWKSAuthService verifies asymmetric tokens (EdDSA/ES256/RS256) minted by
// the boson auth service, using its published JWKS. Keys are cached, refreshed
// periodically, and refetched (at most every 10 s) when a token arrives with
// an unknown kid (key rotation).
type JWKSAuthService struct {
	keys     *jwks.Cache
	issuers  []string // any of these (empty = don't check)
	audience string
}

// NewJWKSAuthService starts the JWKS cache. jwksURLs and issuers are
// comma-separated lists: e.g. the auth service's JWKS and quicvoiced's
// video-ticket JWKS (keys are matched by kid). A JWKS endpoint that is down
// at startup isn't fatal: it's retried in the background (1 s backoff up to
// 30 s) and verification fails until it loads.
func NewJWKSAuthService(ctx context.Context, jwksURLs, issuers, audience string) (*JWKSAuthService, error) {
	urls := splitList(jwksURLs)
	if len(urls) == 0 {
		return nil, errors.New("auth: JWKS URL is empty")
	}
	// keyfunc's defaults (one fetch at boot, then hourly, or once per 5 min
	// on an unknown kid) locked everyone out for minutes when the JWKS was
	// down at boot. jwks.Cache retries the first load with backoff,
	// refetches an unknown kid at most every 10 s, and lets tokens arriving
	// during that fetch wait for it.
	k, err := jwks.New(ctx, urls, jwks.Options{
		RefreshInterval: 15 * time.Minute,
		Logger:          logger.FromCtx(ctx),
	})
	if err != nil {
		return nil, fmt.Errorf("auth: JWKS %s: %w", jwksURLs, err)
	}
	return &JWKSAuthService{keys: k, issuers: splitList(issuers), audience: audience}, nil
}

// Ready reports whether every JWKS has loaded.
func (s *JWKSAuthService) Ready() bool { return s.keys.Ready() }

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *JWKSAuthService) Verify(_ context.Context, token string) (*Identity, error) {
	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"EdDSA", "ES256", "RS256"}),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30 * time.Second),
	}
	if s.audience != "" {
		opts = append(opts, jwt.WithAudience(s.audience))
	}
	var c Claims
	// An unknown kid (key rotation, or the JWKS not loaded yet) is refetched
	// by the cache; concurrent tokens wait for that fetch.
	_, err := jwt.ParseWithClaims(token, &c, s.keys.Keyfunc, opts...)
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
