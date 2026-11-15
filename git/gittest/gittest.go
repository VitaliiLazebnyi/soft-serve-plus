// Package gittest builds commit objects for tests, including malformed ones
// that git accepts in a push but git-module's parser cannot read.
package gittest

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Kind is the shape of a commit object Commit writes.
type Kind string

// The kinds of commit object Commit writes. Every kind but Valid makes
// git-module's parser panic or leave the author unset; git itself lists
// them all.
const (
	// Valid is a well-formed commit.
	Valid Kind = "valid"
	// HeaderNoSpace has a header line without a space.
	HeaderNoSpace Kind = "header-no-space"
	// AuthorNoEmail has an author without "<email>".
	AuthorNoEmail Kind = "author-no-email"
	// AuthorNoDate has an author without a date.
	AuthorNoDate Kind = "author-no-date"
	// NoAuthor has no author line.
	NoAuthor Kind = "no-author"
)

// Malformed lists every kind except Valid.
var Malformed = []Kind{HeaderNoSpace, AuthorNoEmail, AuthorNoDate, NoAuthor}

// Git runs git with args in dir, with no system or global configuration,
// and returns its trimmed output. stdin may be nil.
func Git(t testing.TB, dir string, stdin []byte, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// Tree writes a tree holding one file, name, with content, into the
// repository at dir and returns its ID.
func Tree(t testing.TB, dir, name, content string) string {
	t.Helper()
	blob := Git(t, dir, []byte(content), "hash-object", "-w", "--stdin")
	return Git(t, dir, fmt.Appendf(nil, "100644 blob %s\t%s\n", blob, name), "mktree")
}

// Commit writes a commit object of the given kind with tree and parents
// into the repository at dir, as "git hash-object --literally" does for a
// client that pushes it, and returns its ID. message is its message.
func Commit(t testing.TB, dir string, kind Kind, tree, message string, parents ...string) string {
	t.Helper()
	const (
		author    = "author A U Thor <author@example.com> 1700000000 +0000\n"
		committer = "committer C O Mitter <committer@example.com> 1700000000 +0000\n"
	)
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	switch kind {
	case Valid:
		b.WriteString(author + committer)
	case HeaderNoSpace:
		b.WriteString(author + committer + "nospace\n")
	case AuthorNoEmail:
		b.WriteString("author A U Thor 1700000000 +0000\n" + committer)
	case AuthorNoDate:
		b.WriteString("author A U Thor <author@example.com>\n" + committer)
	case NoAuthor:
		b.WriteString(committer)
	default:
		t.Fatalf("unknown commit kind %q", kind)
	}
	fmt.Fprintf(&b, "\n%s\n", message)
	return Git(t, dir, []byte(b.String()), "hash-object", "-t", "commit", "--literally", "-w", "--stdin")
}
