package git

import (
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Attribute represents a Git attribute.
type Attribute struct {
	Name  string
	Value string
}

// CheckAttributes checks the attributes of the given ref and path.
func (r *Repository) CheckAttributes(ref *Reference, path string) ([]Attribute, error) {
	rnd := rand.NewSource(time.Now().UnixNano())
	fn := "soft-serve-index-" + strconv.Itoa(rand.New(rnd).Int()) //nolint: gosec
	tmpindex := filepath.Join(os.TempDir(), fn)

	defer os.Remove(tmpindex) //nolint: errcheck

	readTree := NewCommand("read-tree", "--reset", "-i", ref.Name().String()).
		AddEnvs("GIT_INDEX_FILE=" + tmpindex)
	if _, err := readTree.RunInDir(r.Path); err != nil {
		return nil, err
	}

	checkAttr := NewCommand("check-attr", "--cached", "-z", "-a", "--", path).
		AddEnvs("GIT_INDEX_FILE=" + tmpindex)
	out, err := checkAttr.RunInDir(r.Path)
	if err != nil {
		return nil, err
	}

	return parseAttributes(out), nil
}

// parseAttributes parses the output of git check-attr -z, which is a list of
// NUL-terminated "<path> NUL <attribute> NUL <info> NUL" triples. The path is
// never quoted there, so non-ASCII and special file names parse as is.
func parseAttributes(buf []byte) []Attribute {
	attrs := make([]Attribute, 0)
	fields := strings.Split(string(buf), "\x00")
	for i := 0; i+2 < len(fields); i += 3 {
		attrs = append(attrs, Attribute{
			Name:  fields[i+1],
			Value: fields[i+2],
		})
	}

	return attrs
}
