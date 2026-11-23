//go:build !windows

package backend

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeChattyGit stands in for a git clone whose remote writes megabytes of
// "remote:" lines, which git relays to its stderr even with --quiet, and
// then fails.
const fakeChattyGit = `#!/bin/sh
i=0
while [ $i -lt 2048 ]; do
	printf 'remote: %01020d\n' $i >&2
	printf 'out: %01020d\n' $i
	i=$((i+1))
done
exit 128
`

// What a remote writes during an import is kept only up to a cap, like a
// mirror sync's output, rather than buffered in full and logged.
func TestCloneImportCapsOutput(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fakeChattyGit), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := cloneImport(t.Context(), importSpec{
		remote: "https://example.invalid/x.git",
		path:   filepath.Join(t.TempDir(), "x.git"),
	})
	if err == nil {
		t.Fatal("cloneImport succeeded")
	}
	msg := err.Error()
	if len(msg) > 16<<10 {
		t.Errorf("error holds %d bytes of the remote's output, want it capped", len(msg))
	}
	if !strings.Contains(msg, "remote: ") {
		t.Errorf("error %.200q... keeps none of git's output", msg)
	}
}

// fakeURLGit prints $OFFSET bytes, then a URL with a token as its user
// name, then more than the cap keeps, and fails, as a remote whose
// "remote:" lines quote such a URL would make it.
const fakeURLGit = `#!/bin/sh
head -c "$OFFSET" /dev/zero | tr '\0' x
printf ' https://ghp_SECRETTOKEN@github.com/o/r.git\n' >&2
i=0
while [ $i -lt 256 ]; do
	printf 'remote: %060d\n' $i >&2
	i=$((i+1))
done
exit 128
`

// A URL the cap cuts must not leave part of its token behind: cut just
// before "@", the rest reads as a URL whose host is the token, which
// redaction keeps.
func TestCloneImportCutURLKeepsNoToken(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fakeURLGit), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The token starts at byte offset+9; put the cut a few bytes into it,
	// around each size the output is cut at.
	for _, limit := range []int{2 << 10, 4 << 10} {
		for _, into := range []int{1, 4, 10, 15} {
			offset := limit - 9 - into
			t.Run(strconv.Itoa(offset), func(t *testing.T) {
				t.Setenv("OFFSET", strconv.Itoa(offset))
				err := cloneImport(t.Context(), importSpec{
					remote: "https://example.invalid/x.git",
					path:   filepath.Join(t.TempDir(), "x.git"),
				})
				if err == nil {
					t.Fatal("cloneImport succeeded")
				}
				if strings.Contains(err.Error(), "ghp_") {
					t.Errorf("error keeps part of the token: %q", err.Error())
				}
			})
		}
	}
}
