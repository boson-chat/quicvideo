// Package auth verifies the tokens clients present in Hello.
package auth

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrUnauthenticated = errors.New("auth: invalid token")
	ErrTokenExpired    = errors.New("auth: token expired")
)

// Role is the token's `role` claim. Same names as quicvoice.
type Role uint8

const (
	RoleListener Role = iota + 1
	RoleSpeaker
	RoleModerator
	RoleAdmin
)

// Video permissions carried in the `perms` claim. Others are ignored.
const (
	PermVideoPublish = "video.publish"
)

// Identity is who a connection is, as asserted by the token issuer.
type Identity struct {
	UserID      string
	DisplayName string
	Role        Role
	// Rooms the token allows joining. Empty = any room.
	Rooms []string
	// Permissions from the `perms` claim. HasPerms: the claim was present,
	// even as [].
	Perms    []string
	HasPerms bool
}

func (i *Identity) CanJoin(room string) bool {
	return len(i.Rooms) == 0 || slices.Contains(i.Rooms, room)
}

// CanPublish: the perms claim decides when present (video.publish),
// otherwise the role (speaker and up).
func (i *Identity) CanPublish() bool {
	if i.HasPerms {
		return slices.Contains(i.Perms, PermVideoPublish)
	}
	return i.Role >= RoleSpeaker
}

type AuthServiceImpl interface {
	Verify(ctx context.Context, token string) (*Identity, error)
}

// Claims as minted by the boson auth service (EdDSA via JWKS) or
// `quicvideod token` (HS256).
type Claims struct {
	Name  string    `json:"name"`
	Role  string    `json:"role"`  // listener | speaker | moderator | admin
	Rooms []string  `json:"rooms"` // allowed rooms; empty = any
	Perms *[]string `json:"perms,omitempty"`
	jwt.RegisteredClaims
}

type JWTAuthService struct {
	secret []byte
}

func NewJWTAuthService(secret string) AuthServiceImpl {
	return &JWTAuthService{secret: []byte(secret)}
}

func (s *JWTAuthService) Verify(_ context.Context, token string) (*Identity, error) {
	var c Claims
	_, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) { return s.secret, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	return identityFromClaims(&c, err)
}

func identityFromClaims(c *Claims, err error) (*Identity, error) {
	if errors.Is(err, jwt.ErrTokenExpired) {
		return nil, ErrTokenExpired
	}
	if err != nil || c.Subject == "" {
		return nil, ErrUnauthenticated
	}
	name := c.Name
	if name == "" {
		name = c.Subject
	}
	id := &Identity{UserID: c.Subject, DisplayName: name, Role: ParseRole(c.Role), Rooms: c.Rooms}
	if c.Perms != nil {
		id.HasPerms = true
		for _, p := range *c.Perms {
			if p = strings.TrimSpace(p); p != "" {
				id.Perms = append(id.Perms, p)
			}
		}
	}
	return id, nil
}

// InsecureAuthService accepts any non-empty token; the token is the user id.
// "name@role" sets the role, e.g. "alice@listener". Dev/testing only.
type InsecureAuthService struct{}

func NewInsecureAuthService() AuthServiceImpl { return InsecureAuthService{} }

func (InsecureAuthService) Verify(_ context.Context, token string) (*Identity, error) {
	if token == "" {
		return nil, ErrUnauthenticated
	}
	user, role, _ := strings.Cut(token, "@")
	return &Identity{UserID: user, DisplayName: user, Role: ParseRole(role)}, nil
}

// ParseRole maps the role claim; unknown/empty = speaker.
func ParseRole(s string) Role {
	switch strings.ToLower(s) {
	case "listener":
		return RoleListener
	case "moderator":
		return RoleModerator
	case "admin":
		return RoleAdmin
	default:
		return RoleSpeaker
	}
}
