package cmd

import (
	"fmt"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/golang-jwt/jwt/v5"
	"github.com/spf13/cobra"
)

// JWTCommand returns a command that generates a JSON Web Token.
func JWTCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jwt REPOSITORY...",
		Short: "Generate a JSON Web Token",
		// The HTTP server only accepts a token whose audience names the
		// repository requested, so a token for no repository is useless.
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			user := proto.UserFromContext(ctx)
			if user == nil {
				return errUnknownKey(ctx)
			}

			cfg := config.FromContext(ctx)
			kp, err := jwk.NewPair(cfg)
			if err != nil {
				return err
			}

			// Mint the spelling the HTTP router checks against.
			audience := make([]string, len(args))
			for i, arg := range args {
				audience[i] = utils.SanitizeRepo(arg)
			}

			now := time.Now()
			expiresAt := now.Add(time.Hour)
			claims := jwt.RegisteredClaims{
				Subject:   fmt.Sprintf("%s#%d", user.Username(), user.ID()),
				ExpiresAt: jwt.NewNumericDate(expiresAt), // expire in an hour
				NotBefore: jwt.NewNumericDate(now),
				IssuedAt:  jwt.NewNumericDate(now),
				Issuer:    cfg.HTTP.PublicURL,
				Audience:  audience,
			}

			token := jwt.NewWithClaims(jwk.SigningMethod, claims)
			token.Header["kid"] = kp.JWK().KeyID
			j, err := token.SignedString(kp.PrivateKey())
			if err != nil {
				return err
			}

			cmd.Println(j)
			return nil
		},
	}

	return cmd
}
