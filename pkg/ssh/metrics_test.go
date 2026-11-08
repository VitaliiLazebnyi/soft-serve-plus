package ssh

import (
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/ssh/cmd"
	"github.com/spf13/cobra"
)

// The CLI command metric is labelled from the server's command tree, never
// with what the client typed, so clients cannot add series.
func TestCommandLabel(t *testing.T) {
	t.Parallel()
	newRoot := func() *cobra.Command {
		root := &cobra.Command{SilenceUsage: true, SilenceErrors: true}
		root.CompletionOptions.DisableDefaultCmd = true
		root.AddCommand(cmd.RepoCommand(), cmd.GitReceivePackCommand())
		return root
	}

	cases := []struct {
		args []string
		want string
	}{
		{nil, ""},
		{[]string{"--help"}, ""},
		{[]string{"repo"}, "repo"},
		{[]string{"repo", "create", "x"}, "repo"},
		{[]string{"repo", "no-such-subcommand"}, "repo"},
		{[]string{"git-receive-pack", "any-name"}, "git-receive-pack"},
		{[]string{"help"}, "help"},
		{[]string{"no-such-command"}, unknownCommand},
		{[]string{strings.Repeat("x", 100<<10)}, unknownCommand},
	}
	for _, tc := range cases {
		if got := commandLabel(newRoot(), tc.args); got != tc.want {
			t.Errorf("commandLabel(%.40q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// Matching the help command early must not stop Execute from running it,
// nor add it twice.
func TestCommandLabelKeepsHelp(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{SilenceUsage: true, SilenceErrors: true}
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(cmd.RepoCommand())
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	args := []string{"help", "repo"}
	if got := commandLabel(root, args); got != "help" {
		t.Fatalf("commandLabel = %q, want help", got)
	}
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute help: %v", err)
	}
	n := 0
	for _, c := range root.Commands() {
		if c.Name() == "help" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("root has %d help commands, want 1", n)
	}
}
