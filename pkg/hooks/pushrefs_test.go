package hooks

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

func TestPushRefs(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	p, err := NewPushRefs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := strings.TrimPrefix(p.Env(), PushRefsEnv+"=")
	if path == p.Env() || filepath.Dir(path) != filepath.Join(cfg.DataPath, "tmp") {
		t.Fatalf("Env() = %q, want %s=<data path>/tmp/...", p.Env(), PushRefsEnv)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && (fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700) {
		t.Errorf("refs file mode %v and directory mode %v, want 0600 and 0700", fi.Mode().Perm(), di.Mode().Perm())
	}

	// Nothing written: no refs.
	refs, err := p.Refs()
	if err != nil || len(refs) != 0 {
		t.Errorf("Refs() of an empty file = %v, %v", refs, err)
	}

	zero := strings.Repeat("0", 40)
	one := strings.Repeat("1", 40)
	sha256 := strings.Repeat("a", 64)
	content := zero + " " + one + " refs/heads/main\n" +
		"\n" +
		"not a ref line\n" +
		one + " " + sha256 + " refs/tags/v1\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	refs, err = p.Refs()
	want := []HookArg{
		{OldSha: zero, NewSha: one, RefName: "refs/heads/main"},
		{OldSha: one, NewSha: sha256, RefName: "refs/tags/v1"},
	}
	if !slices.Equal(refs, want) {
		t.Errorf("Refs() = %v, want %v", refs, want)
	}
	if !errors.Is(err, errBadRefLine) || !strings.Contains(err.Error(), "not a ref line") {
		t.Errorf("Refs() error = %v, want the bad line", err)
	}

	if err := p.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("refs file still there after Remove: %v", err)
	}
	// Removing twice is fine.
	if err := p.Remove(); err != nil {
		t.Errorf("second Remove: %v", err)
	}
}

// Refs files a stopped server left behind are removed at the next start;
// other files in the directory are not. A data path with glob characters in
// its name is a plain path. Regression (review round 2): the sweep globbed
// the whole path, so a data path containing "[x]" matched nothing.
func TestRemoveStalePushRefs(t *testing.T) {
	glob := "data [x] *?"
	if runtime.GOOS == "windows" {
		// Windows file names cannot hold * or ?.
		glob = "data [x]"
	}
	for name, dir := range map[string]string{"plain": "data", "glob characters": glob} {
		t.Run(name, func(t *testing.T) {
			testRemoveStalePushRefs(t, filepath.Join(t.TempDir(), dir))
		})
	}
}

func testRemoveStalePushRefs(t *testing.T, dataPath string) {
	cfg := config.DefaultConfig()
	cfg.DataPath = dataPath
	if n, err := RemoveStalePushRefs(cfg); n != 0 || err != nil {
		t.Fatalf("RemoveStalePushRefs without a directory = %d, %v", n, err)
	}
	var stale []string
	for range 2 {
		p, err := NewPushRefs(cfg)
		if err != nil {
			t.Fatal(err)
		}
		stale = append(stale, strings.TrimPrefix(p.Env(), PushRefsEnv+"="))
	}
	other := filepath.Join(PushRefsDir(cfg), "other")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := RemoveStalePushRefs(cfg)
	if n != 2 || err != nil {
		t.Fatalf("RemoveStalePushRefs = %d, %v, want 2, nil", n, err)
	}
	for _, p := range stale {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still there: %v", p, err)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("other file removed: %v", err)
	}
}

// A push refs directory that already exists with wider permissions, from an
// older server or an operator, is made private to the server's user again.
func TestNewPushRefsTightensDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes mean nothing on Windows")
	}
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	dir := PushRefsDir(cfg)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	p, err := NewPushRefs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Remove() //nolint: errcheck
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("push refs directory mode %v, want 0700", fi.Mode().Perm())
	}
}

func TestIsObjectID(t *testing.T) {
	for s, want := range map[string]bool{
		strings.Repeat("0", 40): true,
		strings.Repeat("f", 64): true,
		strings.Repeat("F", 40): false,
		strings.Repeat("0", 39): false,
		strings.Repeat("g", 40): false,
		"":                      false,
	} {
		if got := isObjectID(s); got != want {
			t.Errorf("isObjectID(%q) = %v, want %v", s, got, want)
		}
	}
}
