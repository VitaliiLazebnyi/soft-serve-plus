package hooks

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
)

func TestGenerateHooks(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = tmp
	repoPath := filepath.Join(tmp, "repos", "test.git")
	_, err := git.Init(repoPath, true)
	if err != nil {
		t.Fatal(err)
	}

	if err := GenerateHooks(context.TODO(), cfg, "test.git"); err != nil {
		t.Fatal(err)
	}

	for _, hn := range []string{
		PreReceiveHook,
		UpdateHook,
		PostReceiveHook,
		PostUpdateHook,
	} {
		if _, err := os.Stat(filepath.Join(repoPath, "hooks", hn)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(repoPath, "hooks", hn+".d", "soft-serve")); err != nil {
			t.Fatal(err)
		}
	}
}

func initRepo(t *testing.T) (*config.Config, string) {
	t.Helper()
	tmp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = tmp
	repoPath := filepath.Join(tmp, "repos", "test.git")
	if _, err := git.Init(repoPath, true); err != nil {
		t.Fatal(err)
	}
	return cfg, repoPath
}

func TestGenerateHooksRestoresExecutableBit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes have no executable bit on Windows")
	}

	cfg, repoPath := initRepo(t)
	if err := GenerateHooks(context.TODO(), cfg, "test.git"); err != nil {
		t.Fatal(err)
	}

	var paths []string
	for _, hn := range []string{PreReceiveHook, UpdateHook, PostReceiveHook, PostUpdateHook} {
		paths = append(paths,
			filepath.Join(repoPath, "hooks", hn),
			filepath.Join(repoPath, "hooks", hn+".d", "soft-serve"),
		)
	}

	// Simulate hooks restored from a backup without the executable bit.
	for _, p := range paths {
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := GenerateHooks(context.TODO(), cfg, "test.git"); err != nil {
		t.Fatal(err)
	}

	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o111 != 0o111 {
			t.Errorf("%s: mode %v is not executable", p, fi.Mode().Perm())
		}
	}
}

func TestGenerateHooksReturnsWriteError(t *testing.T) {
	cfg, repoPath := initRepo(t)

	// A directory in place of the soft-serve hook makes writing it fail.
	if err := os.MkdirAll(filepath.Join(repoPath, "hooks", UpdateHook+".d", "soft-serve"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := GenerateHooks(context.TODO(), cfg, "test.git"); err == nil {
		t.Fatal("expected an error when a hook can't be written")
	}
}
