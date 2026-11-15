package commands

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/boson-chat/quicvideo/server/internal/services/auth"
	"github.com/golang-jwt/jwt/v5"
	"github.com/spf13/cobra"
)

var (
	tokenUser  string
	tokenName  string
	tokenRole  string
	tokenRooms []string
	tokenPerms []string
	tokenTTL   time.Duration
)

// token mints a test token signed with JWT_SECRET. In production the boson
// auth service mints EdDSA tokens verified through AUTH_JWKS_URL.
var tokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Mint a test token (HS256, signed with $JWT_SECRET)",
	RunE: func(cmd *cobra.Command, args []string) error {
		secret := os.Getenv("JWT_SECRET")
		if secret == "" {
			return errors.New("JWT_SECRET is not set")
		}
		if tokenUser == "" {
			return errors.New("--user is required")
		}
		now := time.Now()
		claims := auth.Claims{
			Name:  tokenName,
			Role:  tokenRole,
			Rooms: tokenRooms,
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   tokenUser,
				IssuedAt:  jwt.NewNumericDate(now),
				ExpiresAt: jwt.NewNumericDate(now.Add(tokenTTL)),
			},
		}
		if cmd.Flags().Changed("perm") {
			claims.Perms = &tokenPerms
		}
		s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
		if err != nil {
			return err
		}
		fmt.Println(s)
		return nil
	},
}

func init() {
	tokenCmd.Flags().StringVar(&tokenUser, "user", "", "user id (sub)")
	tokenCmd.Flags().StringVar(&tokenName, "name", "", "display name (defaults to user id)")
	tokenCmd.Flags().StringVar(&tokenRole, "role", "speaker", "listener (watch only) | speaker | moderator | admin")
	tokenCmd.Flags().StringSliceVar(&tokenRooms, "room", nil, "allowed room (repeatable; none = any)")
	tokenCmd.Flags().StringSliceVar(&tokenPerms, "perm", nil, "permission for the perms claim, e.g. video.publish (repeatable; --perm= for an empty claim)")
	tokenCmd.Flags().DurationVar(&tokenTTL, "ttl", time.Hour, "token lifetime")
	rootCmd.AddCommand(tokenCmd)
}
