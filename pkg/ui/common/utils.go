package common

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/muesli/reflow/truncate"
)

// TruncateString is a convenient wrapper around truncate.TruncateString.
func TruncateString(s string, max int) string { //nolint:revive
	if max < 0 {
		max = 0 //nolint:revive
	}
	return truncate.StringWithTail(s, uint(max), "…") //nolint:gosec
}

// RepoURL returns the URL of the repository.
func RepoURL(publicURL, name string) string {
	name = utils.SanitizeRepo(name) + ".git"
	u, err := url.Parse(publicURL)
	if err == nil && u.Scheme == "ssh" {
		host := u.Hostname()
		port := u.Port()
		var user string
		if u.User != nil {
			user = u.User.Username()
		}
		if port == "" || port == "22" {
			if user == "" {
				user = "git"
			}
			// An IPv6 address needs brackets so its colons are not read
			// as the host and path separator of the scp-like syntax.
			if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
			return fmt.Sprintf("%s@%s:%s", user, host, name)
		}
		if user != "" {
			user += "@"
		}
		return fmt.Sprintf("ssh://%s%s/%s", user, net.JoinHostPort(host, port), name)
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
