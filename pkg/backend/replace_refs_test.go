package backend

import (
	"testing"

	"github.com/matryer/is"
)

// A refs/replace ref, which any writer can push and a --mirror import
// fetches, must not change what the server shows: upload-pack ignores it,
// so a clone gets the original objects, and the server's own views must
// show the same. They used to show the replacement.
func TestServerReadsIgnoreReplaceRefs(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	r := readmeRepo(t, map[string]string{"README.md": "real"})
	rr, err := r.Open()
	is.NoErr(err)
	dir := rr.Path
	head := gitIn(t, dir, "", "rev-parse", "HEAD")
	blob := gitIn(t, dir, "fake", "hash-object", "-w", "--stdin")
	tree := gitIn(t, dir, "100644 blob "+blob+"\tREADME.md\n", "mktree")
	fake := gitIn(t, dir, "", "commit-tree", tree, "-m", "fake")
	gitIn(t, dir, "", "update-ref", "refs/replace/"+head, fake)

	readme, _, err := Readme(t.Context(), r, nil, 0)
	is.NoErr(err)
	is.Equal(readme, "real")
}
