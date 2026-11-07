package common

import (
	"fmt"
	"net/url"

	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/charmbracelet/x/ansi"
)

// TruncateString truncates s to at most width terminal cells, ending it with
// "…" when it is cut. It measures with x/ansi, as lipgloss does, so emoji and
// other wide graphemes are counted the same way the layout counts them.
func TruncateString(s string, width int) string {
	return ansi.Truncate(s, max(width, 0), "…")
}

// TruncateWidth truncates s to at most width terminal cells without a tail.
func TruncateWidth(s string, width int) string {
	return ansi.Truncate(s, max(width, 0), "")
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
