package cmd

import (
	"fmt"
	"time"

	"charm.land/log/v2"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/utils"
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

			// Mint the spelling the HTTP router checks against.
			audience := make([]string, len(args))
			for i, arg := range args {
				audience[i] = utils.SanitizeRepo(arg)
			}

			// Signing fails only for the server's host key, whose error
			// names its path on the server.
			j, err := signToken(config.FromContext(ctx), user, audience, time.Now().Add(time.Hour))
			if err != nil {
				return lookupFailed(ctx, log.FromContext(ctx), "read the host key", err)
			}

			cmd.Println(j)
			return nil
		},
	}

	return cmd
}

// signToken signs a token for user with the host key cfg loaded; see
// jwk.Signer.Sign.
func signToken(cfg *config.Config, user proto.User, audience []string, expiresAt time.Time) (string, error) {
	kp, err := config.KeyPair(cfg)
	if err != nil {
		return "", fmt.Errorf("%w: %w", jwk.ErrHostKey, err)
	}
	signer, err := jwk.NewSigner(kp, cfg.HTTP.PublicURL)
	if err != nil {
		return "", err
	}
	return signer.Sign(user, audience, expiresAt)
}
