package git

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
)

// Attribute represents a Git attribute.
type Attribute struct {
	Name  string
	Value string
}

// minAttrSourceVersion is the first git release whose check-attr accepts
// --source, which reads .gitattributes straight from a tree-ish.
var minAttrSourceVersion = Version{Major: 2, Minor: 40}

// attrSource holds whether check-attr supports --source once git's version
// has been read: 0 not yet, 1 yes, 2 no. A failed read is not kept, so a
// passing failure does not stick (see BinVersion).
var attrSource atomic.Int32

// attrSourceSupported reports whether check-attr supports --source. It runs
// `git version`, under ctx, only until it gets an answer: a run that ctx cut
// short is not one. Repository.attrSource holds it, so tests can exercise
// the temporary-index fallback on their own repository.
func attrSourceSupported(ctx context.Context) bool {
	switch attrSource.Load() {
	case 1:
		return true
	case 2:
		return false
	}
	v, err := BinVersion(ctx)
	if err != nil {
		return false
	}
	ok := !v.Less(minAttrSourceVersion)
	if ok {
		attrSource.Store(1)
	} else {
		attrSource.Store(2)
	}
	return ok
}

// CheckAttributes checks the attributes of the given ref and path.
//
// Every git command it runs, including any variant for newer git such as
// `check-attr --source` (git >= 2.40), must be built with
// NewCommand(r.ctx, ...) so that it stops when the SSH
// session that asked for it disconnects.
func (r *Repository) CheckAttributes(ref *Reference, path string) ([]Attribute, error) {
	if r.attrSource(r.ctx) {
		out, err := NewCommand(r.ctx, "check-attr", "-z", "--source", ref.Name().String(), "-a", "--", path).
			RunInDir(r.Path)
		if err != nil {
			return nil, err
		}
		return parseAttributes(out), nil
	}

	// Older git can only read attributes from the working tree or an index,
	// so load the tree into a throwaway index. Keep it in a private directory
	// of its own so other local users cannot predict or pre-create the index
	// or its lock file.
	dir, err := os.MkdirTemp("", "soft-serve-attr-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir) //nolint: errcheck

	env := "GIT_INDEX_FILE=" + filepath.Join(dir, "index")
	readTree := NewCommand(r.ctx, "read-tree", "--reset", "-i", ref.Name().String()).AddEnvs(env)
	if _, err := readTree.RunInDir(r.Path); err != nil {
		return nil, err
	}

	out, err := NewCommand(r.ctx, "check-attr", "-z", "--cached", "-a", "--", path).
		AddEnvs(env).RunInDir(r.Path)
	if err != nil {
		return nil, err
	}

	return parseAttributes(out), nil
}

// parseAttributes parses `git check-attr -z` output, which is a sequence of
// NUL-terminated <path> <attribute> <info> triples. Paths are not quoted in
// this format, so any file name round-trips unchanged.
func parseAttributes(buf []byte) []Attribute {
	attrs := make([]Attribute, 0)
	fields := bytes.Split(buf, []byte{0})
	for i := 0; i+2 < len(fields); i += 3 {
		attrs = append(attrs, Attribute{
			Name:  string(fields[i+1]),
			Value: string(fields[i+2]),
		})
	}

	return attrs
}
