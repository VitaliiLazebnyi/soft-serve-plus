package cmd

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
)

// TokenCommand returns a command that manages user access tokens.
func (cmds *Commands) TokenCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "token",
		Aliases: []string{"access-token"},
		Short:   "Manage access tokens",
	}

	var createExpiresIn string
	createCmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create a new access token",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := cmds.be
			name := strings.Join(args, " ")

			user := proto.UserFromContext(ctx)
			if user == nil {
				return errUnknownKey(ctx)
			}

			var expiresAt time.Time
			if createExpiresIn != "" {
				d, err := parseExpiresIn(createExpiresIn)
				if err != nil {
					return err
				}
				if d <= 0 {
					return fmt.Errorf("invalid --expires-in %q: must be a positive duration", createExpiresIn)
				}

				expiresAt = time.Now().Add(d)
			}

			token, err := be.CreateAccessToken(ctx, user, name, expiresAt)
			if err != nil {
				return err
			}

			notice := "Access token created"
			if !expiresAt.IsZero() {
				notice += " (expires " + humanize.Time(expiresAt) + ")"
			}

			cmd.PrintErrln(notice)
			cmd.Println(token)

			return nil
		},
	}

	createCmd.Flags().StringVar(&createExpiresIn, "expires-in", "", "Token expiration time (e.g. 1y, 3mo, 2w, 5d4h, 1h30m)")

	listCmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List access tokens",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			be := cmds.be

			user := proto.UserFromContext(ctx)
			if user == nil {
				return errUnknownKey(ctx)
			}

			tokens, err := be.ListAccessTokens(ctx, user)
			if err != nil {
				return err
			}

			if len(tokens) == 0 {
				cmd.Println("No tokens found")
				return nil
			}

			now := time.Now()
			tbl := table.New().Headers("ID", "Name", "Created At", "Expires In")
			for _, token := range tokens {
				expiresAt := "-"
				if !token.ExpiresAt.IsZero() {
					if now.After(token.ExpiresAt) {
						expiresAt = "expired"
					} else {
						expiresAt = humanize.Time(token.ExpiresAt)
					}
				}

				tbl = tbl.Row(strconv.FormatInt(token.ID, 10),
					token.Name,
					humanize.Time(token.CreatedAt),
					expiresAt,
				)
			}
			cmd.Println(tbl)
			return nil
		},
	}

	deleteCmd := &cobra.Command{
		Use:     "delete ID",
		Aliases: []string{"rm", "remove"},
		Short:   "Delete an access token",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := cmds.be

			user := proto.UserFromContext(ctx)
			if user == nil {
				return errUnknownKey(ctx)
			}

			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid token ID: %w", err)
			}

			if err := be.DeleteAccessToken(ctx, user, id); err != nil {
				return err
			}

			cmd.PrintErrln("Access token deleted")
			return nil
		},
	}

	cmd.AddCommand(
		createCmd,
		listCmd,
		deleteCmd,
	)

	return cmd
}

// longUnits are the --expires-in units that time.ParseDuration lacks, in
// hours: a day is 24 hours, a week 7 days, a month 30 days and a year 365.
var longUnits = map[string]time.Duration{"d": 24, "w": 7 * 24, "mo": 30 * 24, "y": 365 * 24}

// expiresInPart is one number and unit of an --expires-in value.
var expiresInPart = regexp.MustCompile(`^([0-9]*(?:\.[0-9]*)?)([^0-9.]*)`)

// parseExpiresIn parses a duration as time.ParseDuration does, with the
// units d, w, mo and y added, such as "1y", "3mo" or "5d4h".
func parseExpiresIn(s string) (time.Duration, error) {
	rest, neg := s, false
	if rest != "" && (rest[0] == '-' || rest[0] == '+') {
		neg, rest = rest[0] == '-', rest[1:]
	}
	if rest == "" {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	var total time.Duration
	for rest != "" {
		m := expiresInPart.FindStringSubmatch(rest)
		num, unit := m[1], m[2]
		if num == "" || num == "." {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		rest = rest[len(m[0]):]
		var d time.Duration
		var err error
		if hours, ok := longUnits[unit]; ok {
			d, err = time.ParseDuration(num + "h")
			if err == nil && d > math.MaxInt64/hours {
				err = fmt.Errorf("invalid duration %q", s)
			}
			d *= hours
		} else {
			if unit == "" && num == "0" && rest == "" && total == 0 {
				return 0, nil
			}
			d, err = time.ParseDuration(num + unit)
		}
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: units are ns, us, ms, s, m, h, d, w, mo and y", s)
		}
		if total > math.MaxInt64-d {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		total += d
	}
	if neg {
		total = -total
	}
	return total, nil
}
