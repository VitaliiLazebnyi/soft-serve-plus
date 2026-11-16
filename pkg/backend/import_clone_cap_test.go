//go:build !windows

package backend

import (
	"os"
	"path/filepath"
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
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fakeChattyGit), 0o755); err != nil { //nolint:gosec
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
