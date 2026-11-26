package buildcheck

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

// dbTestPackages are the test helpers that open a database on Postgres when
// SOFT_SERVE_DB_DRIVER is "postgres".
var dbTestPackages = []string{
	"github.com/charmbracelet/soft-serve/internal/dbtest",
	"github.com/charmbracelet/soft-serve/internal/backendtest",
}

// TestPostgresPackagesListsEveryDatabaseTest checks that the coverage job's
// Postgres step runs every package whose tests open a database through
// dbtest, and only those: a package left out runs on SQLite only. The
// scripts have steps of their own.
func TestPostgresPackagesListsEveryDatabaseTest(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", "coverage.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Jobs map[string]struct {
			Env map[string]string `yaml:"env"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	patterns := strings.Fields(w.Jobs["coverage"].Env["POSTGRES_PACKAGES"])
	if len(patterns) == 0 {
		t.Fatal("coverage.yml: no POSTGRES_PACKAGES in the coverage job")
	}

	dirs := databaseTestDirs(t)
	for dir := range dirs {
		if dir == "testscript" {
			continue
		}
		if !matchesAny(patterns, dir) {
			t.Errorf("coverage.yml: POSTGRES_PACKAGES leaves out ./%s, whose tests open a database through dbtest", dir)
		}
	}
	for _, p := range patterns {
		matched := false
		for dir := range dirs {
			if matchesAny([]string{p}, dir) {
				matched = true
			}
		}
		if !matched {
			t.Errorf("coverage.yml: POSTGRES_PACKAGES lists %s, where no test opens a database through dbtest", p)
		}
	}
}

// databaseTestDirs returns the slash-separated directories, relative to the
// repository root, of the packages with a test file that imports one of
// dbTestPackages, or that are one.
func databaseTestDirs(t *testing.T) map[string]bool {
	t.Helper()
	dirs := map[string]bool{}
	err := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// The go tool ignores directories named testdata or starting with
		// "." or "_", so no package there runs in CI.
		if d.IsDir() && p != repoRoot && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_")) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, filepath.Dir(p))
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			ip, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			for _, h := range dbTestPackages {
				if ip == h || "github.com/charmbracelet/soft-serve/"+rel == h {
					dirs[rel] = true
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

// matchesAny reports whether dir is matched by one of the go test patterns,
// each "./dir" or "./dir/...".
func matchesAny(patterns []string, dir string) bool {
	for _, p := range patterns {
		p = strings.TrimPrefix(p, "./")
		if base, ok := strings.CutSuffix(p, "/..."); ok {
			if dir == base || strings.HasPrefix(dir, base+"/") {
				return true
			}
			continue
		}
		if path.Clean(p) == dir {
			return true
		}
	}
	return false
}
