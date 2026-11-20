package hooks

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestPushRefs(t *testing.T) {
	p, err := NewPushRefs()
	if err != nil {
		t.Fatal(err)
	}
	path := strings.TrimPrefix(p.Env(), PushRefsEnv+"=")
	if path == p.Env() || !filepath.IsAbs(path) {
		t.Fatalf("Env() = %q, want %s=<absolute path>", p.Env(), PushRefsEnv)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("refs file mode %v, want 0600", fi.Mode().Perm())
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
