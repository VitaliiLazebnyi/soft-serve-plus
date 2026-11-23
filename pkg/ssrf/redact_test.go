package ssrf

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestRedactURL(t *testing.T) {
	tests := map[string]string{
		"https://user:secret@example.com/x.git":               "https://example.com/x.git",
		"https://TOKEN@example.com/x.git":                     "https://example.com/x.git",
		"https://example.com/x.git":                           "https://example.com/x.git",
		"https://s3.example.com/obj?X-Amz-Signature=SIG#frag": "https://s3.example.com/obj",
		"https://example.com/obj?":                            "https://example.com/obj",
		"ssh://git@10.0.0.1/x.git":                            "ssh://10.0.0.1/x.git",
		"git@github.com:x/y.git":                              "git@github.com:x/y.git",
		"https://TOKEN@example.com/%zz":                       "[redacted]",
		// No host: the user info and query still go.
		"https://u:TOKEN@/path":         "https:///path",
		"https://TOKEN@/path?sig=TOKEN": "https:///path",
		"file:///srv/x.git?token=TOKEN": "file:///srv/x.git",
		// Not URLs: no "://", so no authority and no user info to strip.
		// url.Parse reads whatever comes before the first colon as a
		// scheme, so these must still be left alone.
		"host:path":            "host:path",
		"just some words":      "just some words",
		`C:\x`:                 `C:\x`,
		`C:\x\y?z`:             `C:\x\y?z`,
		"fatal: what? yes #3":  "fatal: what? yes #3",
		"host:path?q#f":        "host:path?q#f",
		"mailto:a@b?subject=x": "mailto:a@b?subject=x",
	}
	for in, want := range tests {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// RedactURLError applies the rule it is given to the URL of a *url.Error,
// keeps the error chain, replaces a parse error's message, and leaves other
// errors alone.
func TestRedactURLError(t *testing.T) {
	in := &url.Error{Op: "Get", URL: "https://USER:PASS@host/a/b?sig=SECRET", Err: context.DeadlineExceeded}
	err := RedactURLError(in, RedactURL)
	if got, want := err.Error(), "Get \"https://host/a/b\": context deadline exceeded"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v lost its chain", err)
	}
	if in.URL != "https://USER:PASS@host/a/b?sig=SECRET" {
		t.Errorf("the original error was changed: %q", in.URL)
	}
	strict := func(string) string { return "[redacted]" }
	if got := RedactURLError(in, strict).Error(); strings.Contains(got, "host") {
		t.Errorf("the rule given was not applied: %q", got)
	}

	_, perr := url.Parse("https://host:SECRETPORT/x") //nolint:staticcheck // invalid on purpose
	rerr := RedactURLError(perr, RedactURL)
	if !errors.Is(rerr, ErrInvalidURL) || strings.Contains(rerr.Error(), "SECRETPORT") {
		t.Errorf("parse error = %q, want ErrInvalidURL without the port", rerr)
	}

	other := errors.New("other")
	if got := RedactURLError(other, RedactURL); got != other { //nolint:errorlint
		t.Errorf("other error = %v, want it unchanged", got)
	}
	if got := RedactURLError(nil, RedactURL); got != nil {
		t.Errorf("nil error = %v", got)
	}
}
