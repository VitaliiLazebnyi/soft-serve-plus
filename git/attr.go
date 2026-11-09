package git

import (
	"bytes"
	"os"
	"path/filepath"
)

// Attribute represents a Git attribute.
type Attribute struct {
	Name  string
	Value string
}

// minAttrSourceVersion is the first git release whose check-attr accepts
// --source, which reads .gitattributes straight from a tree-ish.
var minAttrSourceVersion = Version{Major: 2, Minor: 40}

// attrSourceSupported reports whether check-attr supports --source. It is a
// variable so tests can exercise the temporary-index fallback.
var attrSourceSupported = func() bool {
	v, err := BinVersion()
	return err == nil && !v.Less(minAttrSourceVersion)
}

// CheckAttributes checks the attributes of the given ref and path.
func (r *Repository) CheckAttributes(ref *Reference, path string) ([]Attribute, error) {
	if attrSourceSupported() {
		out, err := NewCommand("check-attr", "-z", "--source", ref.Name().String(), "-a", "--", path).
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
	readTree := NewCommand("read-tree", "--reset", "-i", ref.Name().String()).AddEnvs(env)
	if _, err := readTree.RunInDir(r.Path); err != nil {
		return nil, err
	}

	out, err := NewCommand("check-attr", "-z", "--cached", "-a", "--", path).AddEnvs(env).RunInDir(r.Path)
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
