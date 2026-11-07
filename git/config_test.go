package git

import (
	"os"
	"path/filepath"
	"testing"
)

func initBareRepo(t *testing.T) *Repository {
	t.Helper()
	r, err := Init(filepath.Join(t.TempDir(), "repo"), true)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	return r
}

func TestSetConfigRoundTrip(t *testing.T) {
	t.Parallel()
	r := initBareRepo(t)

	cfg, err := r.Config()
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	cfg.Section("remote").Subsection("origin").SetOption("url", "https://example.com/repo.git")
	if err := r.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	got, err := r.Config()
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if u := got.Section("remote").Subsection("origin").Option("url"); u != "https://example.com/repo.git" {
		t.Fatalf("remote url = %q", u)
	}
	if _, err := os.Stat(filepath.Join(r.Path, "config.lock")); !os.IsNotExist(err) {
		t.Fatalf("config.lock left behind: %v", err)
	}
}

// TestSetConfigHonoursLock checks that SetConfig, like git, refuses to write
// while config.lock is held and leaves both files as they were.
func TestSetConfigHonoursLock(t *testing.T) {
	t.Parallel()
	r := initBareRepo(t)

	cp := filepath.Join(r.Path, "config")
	before, err := os.ReadFile(cp)
	if err != nil {
		t.Fatal(err)
	}
	lp := cp + ".lock"
	if err := os.WriteFile(lp, []byte("held"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := r.Config()
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	cfg.Section("lfs").SetOption("url", "https://example.com/lfs")
	if err := r.SetConfig(cfg); err == nil {
		t.Fatal("SetConfig succeeded while config.lock was held")
	}

	after, err := os.ReadFile(cp)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("config changed while locked:\n%s", after)
	}
	held, err := os.ReadFile(lp)
	if err != nil {
		t.Fatalf("config.lock removed: %v", err)
	}
	if string(held) != "held" {
		t.Fatalf("config.lock overwritten: %q", held)
	}
}
