package admin

import (
	"bufio"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/soft-serve/cmd"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/spf13/cobra"
)

// errRollbackNotConfirmed is returned when "soft admin rollback" is not
// confirmed.
var errRollbackNotConfirmed = errors.New("rollback not confirmed")

var (
	// rollbackYes skips the rollback confirmation prompt.
	rollbackYes bool

	// Command is the admin command.
	Command = &cobra.Command{
		Use:   "admin",
		Short: "Administrate the server",
		// A command without RunE shows its help for any arguments, so an
		// unknown subcommand would exit successfully.
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return c.Help()
		},
	}

	migrateCmd = &cobra.Command{
		Use:                "migrate",
		Short:              "Migrate the database to the latest version",
		Args:               cobra.NoArgs,
		PersistentPreRunE:  cmd.InitBackendContext,
		PersistentPostRunE: cmd.CloseDBContext,
		RunE: cmd.CloseDBOnError(func(c *cobra.Command, _ []string) error {
			ctx := c.Context()
			db := db.FromContext(ctx)
			if err := migrate.Migrate(ctx, db); err != nil {
				return fmt.Errorf("migration: %w", err)
			}

			return nil
		}),
	}

	rollbackCmd = &cobra.Command{
		Use:   "rollback",
		Short: "Rollback the database to the previous version",
		Long: "Rollback the database to the previous version.\n\n" +
			"This undoes the last database migration and may drop the data it added. " +
			"It asks for confirmation unless --yes is given.",
		Args:               cobra.NoArgs,
		PersistentPreRunE:  cmd.InitBackendContext,
		PersistentPostRunE: cmd.CloseDBContext,
		RunE: cmd.CloseDBOnError(func(c *cobra.Command, _ []string) error {
			if !rollbackYes && !confirm(c, "Roll back the last database migration? This may drop data. [y/N] ") {
				return errRollbackNotConfirmed
			}

			ctx := c.Context()
			db := db.FromContext(ctx)
			if err := migrate.Rollback(ctx, db); err != nil {
				return fmt.Errorf("rollback: %w", err)
			}

			return nil
		}),
	}

	syncHooksCmd = &cobra.Command{
		Use:                "sync-hooks",
		Short:              "Update repository hooks",
		Args:               cobra.NoArgs,
		PersistentPreRunE:  cmd.InitBackendContext,
		PersistentPostRunE: cmd.CloseDBContext,
		RunE: cmd.CloseDBOnError(func(c *cobra.Command, _ []string) error {
			ctx := c.Context()
			cfg := config.FromContext(ctx)
			be := backend.FromContext(ctx)
			if err := cmd.InitializeHooks(ctx, cfg, be); err != nil {
				return fmt.Errorf("initialize hooks: %w", err)
			}

			return nil
		}),
	}
)

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

func init() {
	rollbackCmd.Flags().BoolVarP(&rollbackYes, "yes", "y", false, "roll back without asking for confirmation")
	Command.AddCommand(
		syncHooksCmd,
		migrateCmd,
		rollbackCmd,
	)
}
