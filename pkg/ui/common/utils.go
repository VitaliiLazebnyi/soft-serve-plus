package common

import (
	"fmt"
	"net/url"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/muesli/reflow/truncate"
)

// TruncateString is a convenient wrapper around truncate.TruncateString.
func TruncateString(s string, width int) string {
	if width < 0 {
		width = 0
	}
	return truncate.StringWithTail(s, uint(width), "…") //nolint:gosec
}

// RepoURL returns the URL of the repository.
func RepoURL(publicURL, name string) string {
	name = utils.SanitizeRepo(name) + ".git"
	url, err := url.Parse(publicURL)
	if err == nil {
		switch url.Scheme {
		case "ssh":
			port := url.Port()
			if port == "" || port == "22" {
				return fmt.Sprintf("git@%s:%s", url.Hostname(), name)
			}
			return fmt.Sprintf("ssh://%s:%s/%s", url.Hostname(), url.Port(), name)
		}
	}

	return fmt.Sprintf("%s/%s", publicURL, name)
}

// VisibilityMarker returns the marker shown after a repository's name: a
// lock for a private repository and a globe for a public one. Both are
// shown so a public repository cannot be mistaken for a private one.
func VisibilityMarker(private bool) string {
	if private {
		return "🔒"
	}
	return "🌐"
}

// IsInputMsg reports whether msg is user input: a key, mouse or paste event.
func IsInputMsg(msg tea.Msg) bool {
	switch msg.(type) {
	case tea.KeyMsg, tea.MouseMsg, tea.PasteMsg, tea.PasteStartMsg, tea.PasteEndMsg:
		return true
	}
	return false
}
