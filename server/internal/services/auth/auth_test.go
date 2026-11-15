package auth

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func mint(t *testing.T, secret string, c Claims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestJWTVerify(t *testing.T) {
	svc := NewJWTAuthService("s3cret")
	exp := jwt.NewNumericDate(time.Now().Add(time.Hour))
	tok := mint(t, "s3cret", Claims{Name: "Alice", Rooms: []string{"a"}, RegisteredClaims: jwt.RegisteredClaims{Subject: "u1", ExpiresAt: exp}})
	id, err := svc.Verify(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if id.UserID != "u1" || id.DisplayName != "Alice" || !id.CanJoin("a") || id.CanJoin("b") || !id.CanPublish() {
		t.Fatalf("bad identity %+v", id)
	}
	if _, err := svc.Verify(context.Background(), mint(t, "other", Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "u1", ExpiresAt: exp}})); err != ErrUnauthenticated {
		t.Fatalf("wrong secret: %v", err)
	}
	old := jwt.NewNumericDate(time.Now().Add(-time.Hour))
	if _, err := svc.Verify(context.Background(), mint(t, "s3cret", Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "u1", ExpiresAt: old}})); err != ErrTokenExpired {
		t.Fatalf("expired: %v", err)
	}
}

func TestCanPublish(t *testing.T) {
	none := []string{}
	pub := []string{"voice.create", PermVideoPublish}
	cases := []struct {
		id   Identity
		want bool
	}{
		{Identity{Role: RoleListener}, false},
		{Identity{Role: RoleSpeaker}, true},
		{Identity{Role: RoleAdmin, HasPerms: true, Perms: none}, false},
		{Identity{Role: RoleListener, HasPerms: true, Perms: pub}, true},
	}
	for i, c := range cases {
		if got := c.id.CanPublish(); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

func TestInsecure(t *testing.T) {
	id, err := NewInsecureAuthService().Verify(context.Background(), "bob@listener")
	if err != nil || id.UserID != "bob" || id.CanPublish() {
		t.Fatalf("got %+v %v", id, err)
	}
}
