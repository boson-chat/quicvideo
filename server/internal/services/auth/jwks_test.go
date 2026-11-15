package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// jwksServer serves one Ed25519 key as a JWKS.
func jwksServer(t *testing.T, kid string) (ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": kid,
			"x": base64.RawURLEncoding.EncodeToString(pub),
		}}})
	}))
	t.Cleanup(srv.Close)
	return priv, srv.URL
}

func sign(t *testing.T, key ed25519.PrivateKey, kid, iss string, rooms []string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, Claims{Name: "Alice", Rooms: rooms, RegisteredClaims: jwt.RegisteredClaims{
		Subject: "u1", Issuer: iss, Audience: jwt.ClaimStrings{"quicvideo"},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
	}})
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The auth service's tokens and quicvoiced's video tickets side by side.
func TestJWKSMultipleSourcesAndIssuers(t *testing.T) {
	authKey, authURL := jwksServer(t, "auth-1")
	voiceKey, voiceURL := jwksServer(t, "voice-1")
	svc, err := NewJWKSAuthService(context.Background(), authURL+", "+voiceURL, "https://auth.example, voice", "quicvideo")
	if err != nil {
		t.Fatal(err)
	}
	if id, err := svc.Verify(context.Background(), sign(t, authKey, "auth-1", "https://auth.example", nil)); err != nil || id.UserID != "u1" {
		t.Fatalf("auth token: %v", err)
	}
	id, err := svc.Verify(context.Background(), sign(t, voiceKey, "voice-1", "voice", []string{"general"}))
	if err != nil || !id.CanJoin("general") || id.CanJoin("other") {
		t.Fatalf("voice ticket: %+v %v", id, err)
	}
	if _, err := svc.Verify(context.Background(), sign(t, voiceKey, "voice-1", "elsewhere", nil)); err == nil {
		t.Fatal("an unlisted issuer must be rejected")
	}
	stranger, _ := jwksServer(t, "x")
	if _, err := svc.Verify(context.Background(), sign(t, stranger, "voice-1", "voice", nil)); err == nil {
		t.Fatal("a key that isn't in either JWKS must be rejected")
	}
}
