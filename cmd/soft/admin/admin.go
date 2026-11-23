// Package admin is the soft admin command: database migrations and
// rollbacks, and regenerating the hooks of every repository.
package admin

import (
	"bufio"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/soft-serve/cmd"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/spf13/cobra"
)

// errRollbackNotConfirmed is returned when "soft admin rollback" is not
// confirmed.
var errRollbackNotConfirmed = errors.New("rollback not confirmed")

// NewCommand returns a new admin command with its subcommands, which load
// the server's config and logger with load (see cmd.OpenEnv).
//
// Each call builds a new command tree, so its flags start from their
// defaults: cobra does not reset a flag a previous run set.
func NewCommand(load cmd.Loader) *cobra.Command {
	command := &cobra.Command{
		Use:   "admin",
		Short: "Administrate the server",
		// A command without RunE shows its help for any arguments, so an
		// unknown subcommand would exit successfully.
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return c.Help()
		},
	}

	migrateCmd := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate the database to the latest version",
		Args:  cobra.NoArgs,
		RunE: cmd.WithEnv(load, false, func(c *cobra.Command, _ []string, env *cmd.Env) error {
			if err := migrate.Migrate(c.Context(), env.DB, env.Config, env.Logger); err != nil {
				return fmt.Errorf("migration: %w", err)
			}

			return nil
		}),
	}

	// rollbackYes skips the rollback confirmation prompt.
	var rollbackYes bool
	rollbackCmd := &cobra.Command{
		Use:   "rollback",
		Short: "Rollback the database to the previous version",
		Long: "Rollback the database to the previous version.\n\n" +
			"This undoes the last database migration and may drop the data it added. " +
			"It asks for confirmation unless --yes is given.",
		Args: cobra.NoArgs,
		RunE: cmd.WithEnv(load, false, func(c *cobra.Command, _ []string, env *cmd.Env) error {
			if !rollbackYes && !confirm(c, "Roll back the last database migration? This may drop data. [y/N] ") {
				return errRollbackNotConfirmed
			}

			if err := migrate.Rollback(c.Context(), env.DB, env.Config, env.Logger); err != nil {
				return fmt.Errorf("rollback: %w", err)
			}

			return nil
		}),
	}
	rollbackCmd.Flags().BoolVarP(&rollbackYes, "yes", "y", false, "roll back without asking for confirmation")

	syncHooksCmd := &cobra.Command{
		Use:   "sync-hooks",
		Short: "Update repository hooks",
		Args:  cobra.NoArgs,
		RunE: cmd.WithEnv(load, false, func(c *cobra.Command, _ []string, env *cmd.Env) error {
			if err := cmd.InitializeHooks(c.Context(), env.Config, env.Backend, env.Logger); err != nil {
				return fmt.Errorf("initialize hooks: %w", err)
			}

			return nil
		}),
	}

	command.AddCommand(
		syncHooksCmd,
		migrateCmd,
		rollbackCmd,
	)

	return command
}

// confirm writes prompt to c's error output and reports whether the line
// read from c's input is "y" or "yes". No input, as when stdin is not a
// terminal, is a no.
func confirm(c *cobra.Command, prompt string) bool {
	fmt.Fprint(c.ErrOrStderr(), prompt) //nolint: errcheck
	line, _ := bufio.NewReader(c.InOrStdin()).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}
