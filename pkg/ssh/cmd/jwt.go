package cmd

import (
	"time"

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/spf13/cobra"
)

// JWTCommand returns a command that generates a JSON Web Token.
func (cmds *Commands) JWTCommand() *cobra.Command {
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
				audience[i] = proto.SanitizeRepo(arg)
			}

			// Signing fails only for the server's host key, whose error
			// names its path on the server.
			j, err := cmds.tokens.Sign(user, audience, time.Now().Add(time.Hour))
			if err != nil {
				return lookupFailed(ctx, cmds.logger, "read the host key", err)
			}

			cmd.Println(j)
			return nil
		},
	}

	return cmd
}
