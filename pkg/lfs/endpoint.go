package lfs

import (
	"errors"
	"net/url"
	"strings"
)

// Endpoint is a Git LFS endpoint.
type Endpoint = *url.URL

// ErrUnsupportedEndpoint is returned by NewEndpoint for a remote objects
// can't be fetched from: anything but http, https and git:// URLs, which
// includes ssh and scp-style remotes, as Git LFS over SSH is only served, not
// fetched from.
var ErrUnsupportedEndpoint = errors.New("lfs: objects are fetched over http or https only")

// ErrInvalidEndpoint is returned by NewEndpoint for a string that does not
// parse as a URL. It does not quote the string, which can hold credentials.
var ErrInvalidEndpoint = errors.New("lfs: endpoint is not a valid url")

// NewEndpoint returns a new Git LFS endpoint. rawurl is either a Git remote,
// whose endpoint is derived from it, or an http(s) URL whose path already ends
// in /info/lfs, which is used as is. Any other remote fails with
// ErrUnsupportedEndpoint, and a string that does not parse with
// ErrInvalidEndpoint; an scp-style remote is one of those, so callers that
// need to tell it apart ask pkg/ssrf, which reads remotes as git does.
func NewEndpoint(rawurl string) (Endpoint, error) {
	// The parse error is not kept: it quotes the URL, which can hold
	// credentials.
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, ErrInvalidEndpoint
	}

	u.Path = strings.TrimSuffix(u.Path, "/")

	switch u.Scheme {
	case "git":
		// Use https for git:// URLs and strip the port if it exists,
		// keeping the brackets around an IPv6 literal.
		u.Scheme = "https"
		if port := u.Port(); port != "" {
			u.Host = strings.TrimSuffix(u.Host, ":"+port)
		}
		fallthrough
	case "http", "https":
		switch {
		case strings.HasSuffix(u.Path, "/info/lfs"):
			// Already an LFS endpoint (git-lfs's lfs.url), not a remote.
		case strings.HasSuffix(u.Path, ".git"):
			u.Path += "/info/lfs"
		default:
			u.Path += ".git/info/lfs"
		}
	default:
		return nil, ErrUnsupportedEndpoint
	}

	return u, nil
}
