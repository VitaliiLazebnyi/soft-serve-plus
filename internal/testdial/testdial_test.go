package testdial

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// importPath is this package's import path.
const importPath = "github.com/charmbracelet/soft-serve/internal/testdial"

// readOnly is what code outside tests may use from this package: reading the
// hooks, which are nil unless a test set them.
var readOnly = map[string]bool{"Load": true, "LoadResolver": true}

// TestHooksSetOnlyByTests checks that no non-test file in the module can turn
// the hooks on. pkg/ssrf reads them in production code, so a release binary
// links this package, but with nothing that sets them the linker drops Set,
// SetResolver, NoDNS and Network, and the hooks stay nil.
func TestHooksSetOnlyByTests(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	readers := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		name := ""
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == importPath {
				name = "testdial"
				if imp.Name != nil {
					name = imp.Name.Name
				}
				if name == "." {
					t.Errorf("%s: non-test code dot-imports testdial", fset.Position(imp.Pos()))
					return nil
				}
			}
		}
		if name == "" {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == name {
				if !readOnly[sel.Sel.Name] {
					t.Errorf("%s: non-test code uses testdial.%s", fset.Position(sel.Pos()), sel.Sel.Name)
				}
				readers++
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if readers == 0 {
		t.Fatal("found no reader of the hooks; is the walk looking at the module?")
	}
}
