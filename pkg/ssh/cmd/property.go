package cmd

import (
	"context"
	"strconv"
	"strings"

	"github.com/charmbracelet/soft-serve/internal/termtext"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/spf13/cobra"
)

// repoProperty is a repository setting that a command of its own prints,
// given only the repository, or changes, given a value as well. Exactly one
// of setText and setBool is set.
type repoProperty struct {
	use     string
	aliases []string
	short   string
	// need is the access level that changing the property takes. Reading
	// it takes read access.
	need access.Level
	get  func(proto.Repository) string
	// setText stores a free-text value: the arguments after the
	// repository, joined by spaces.
	setText func(ctx context.Context, be *backend.Backend, repo proto.Repository, value string) error
	// setBool stores a boolean value, one argument.
	setBool func(ctx context.Context, be *backend.Backend, repo proto.Repository, value bool) error
}

// repoProperties are the property commands of `repo`. Each setter acts on
// the repository the command checked (Backend.SetResolvedHidden and the
// like), not whatever repository has its name by then; the mirror flag has
// no such setter yet and is set by name.
//
// Changing the visibility, the hidden flag or the mirror flag takes admin
// access: making a private repository public exposes it to everyone and a
// public one private locks out its readers; hiding takes a repository out
// of every listing; and mirroring points the repository at a remote that
// replaces its branches and tags on every sync, while turning it off lets
// pushes in again.
var repoProperties = []repoProperty{
	{
		use:     "description REPOSITORY [DESCRIPTION]",
		aliases: []string{"desc"},
		short:   "Set or get the description for a repository",
		need:    access.ReadWriteAccess,
		get:     func(r proto.Repository) string { return termtext.Sanitize(r.Description()) },
		setText: func(ctx context.Context, be *backend.Backend, r proto.Repository, v string) error {
			return be.SetResolvedDescription(ctx, r, v)
		},
	},
	{
		use:     "hidden REPOSITORY [true|false]",
		aliases: []string{"hide"},
		short:   "Hide or unhide a repository",
		need:    access.AdminAccess,
		get:     func(r proto.Repository) string { return strconv.FormatBool(r.IsHidden()) },
		setBool: func(ctx context.Context, be *backend.Backend, r proto.Repository, v bool) error {
			return be.SetResolvedHidden(ctx, r, v)
		},
	},
	{
		use:   "mirror REPOSITORY [true|false]",
		short: "Set or get a repository mirror property",
		need:  access.AdminAccess,
		get:   func(r proto.Repository) string { return strconv.FormatBool(r.IsMirror()) },
		setBool: func(ctx context.Context, be *backend.Backend, r proto.Repository, v bool) error {
			return be.SetMirror(ctx, r.Name(), v)
		},
	},
	{
		use:   "private REPOSITORY [true|false]",
		short: "Set or get a repository private property",
		need:  access.AdminAccess,
		get:   func(r proto.Repository) string { return strconv.FormatBool(r.IsPrivate()) },
		setBool: func(ctx context.Context, be *backend.Backend, r proto.Repository, v bool) error {
			return be.SetResolvedPrivate(ctx, r, v)
		},
	},
	{
		use:     "project-name REPOSITORY [NAME]",
		aliases: []string{"project"},
		short:   "Set or get the project name for a repository",
		need:    access.ReadWriteAccess,
		get:     func(r proto.Repository) string { return termtext.Sanitize(r.ProjectName()) },
		setText: func(ctx context.Context, be *backend.Backend, r proto.Repository, v string) error {
			return be.SetResolvedProjectName(ctx, r, v)
		},
	},
}

// propertyCommand returns the command that prints or changes p.
func (cmds *Commands) propertyCommand(p repoProperty) *cobra.Command {
	args := cobra.RangeArgs(1, 2)
	if p.setText != nil {
		args = cobra.MinimumNArgs(1)
	}
	return &cobra.Command{
		Use:     p.use,
		Aliases: p.aliases,
		Short:   p.short,
		Args:    args,
		RunE: cmds.withRepo(access.ReadOnlyAccess, func(cmd *cobra.Command, args []string, repo proto.Repository, level access.Level) error {
			if len(args) == 1 {
				cmd.Println(p.get(repo))
				return nil
			}
			if level < p.need {
				return proto.ErrUnauthorized
			}
			ctx := cmd.Context()
			be := cmds.be
			if p.setText != nil {
				return p.setText(ctx, be, repo, strings.Join(args[1:], " "))
			}
			// Anything but a boolean is refused rather than read as false,
			// which a typo such as "yes" would otherwise mean.
			v, err := parseBool(args[1])
			if err != nil {
				return err
			}
			return p.setBool(ctx, be, repo, v)
		}),
	}
}
