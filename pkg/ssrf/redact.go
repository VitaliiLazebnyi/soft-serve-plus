package ssrf

import (
	"net/url"
	"strings"
)

// RedactURL returns rawURL without its user info, query and fragment, so it
// can be logged or returned in an error. Remotes and LFS endpoints can carry
// a token as the user name or password, and LFS hrefs are often presigned
// URLs with a signature in the query. A URL that cannot be parsed is
// replaced entirely; a string without "://" (such as an scp-style remote, a
// Windows path or plain text) is returned unchanged.
func RedactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		if strings.Contains(rawURL, "://") {
			return "[redacted]"
		}
		return rawURL
	}
	// A string without "://" has no authority, so no user info to strip:
	// an scp-style remote, a Windows path, plain text with a colon, which
	// url.Parse reads as a scheme. With "://", the user info and query go
	// even when the host is empty: "https://TOKEN@/x" parses with the token
	// as user info.
	if u.Scheme == "" || !strings.Contains(rawURL, "://") {
		return rawURL
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// RedactURLError redacts the URL of a *url.Error, which the HTTP client
// returns for every failed request and which prints the whole URL except for
// a password, with redact: RedactURL, or a stricter rule where the path can
// hold a secret too, as in many webhook URLs. The returned error keeps the
// operation and the underlying error (except for a parse error, whose
// message can quote part of the URL and is replaced with ErrInvalidURL), so
// errors.Is and errors.As (context.DeadlineExceeded, net.Error timeouts and
// so on) still work. Any other error is returned unchanged.
func RedactURLError(err error, redact func(string) string) error {
	uerr, ok := err.(*url.Error) // only the top-level error is replaced
	if !ok {
		return err
	}
	inner := uerr.Err
	if uerr.Op == "parse" {
		inner = ErrInvalidURL
	}
	return &url.Error{Op: uerr.Op, URL: redact(uerr.URL), Err: inner}
}
