package git

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestRenameRetry(t *testing.T) {
	t.Parallel()
	errBusy := errors.New("busy")
	errOther := errors.New("other")
	retryable := func(err error) bool { return errors.Is(err, errBusy) }
	policy := renameRetryPolicy{first: time.Millisecond, total: 50 * time.Millisecond}

	t.Run("succeeds after transient failures", func(t *testing.T) {
		t.Parallel()
		calls := 0
		err := renameRetry("a", "b", func(string, string) error {
			calls++
			if calls < 3 {
				return errBusy
			}
			return nil
		}, retryable, policy)
		if err != nil || calls != 3 {
			t.Fatalf("err = %v, calls = %d; want nil, 3", err, calls)
		}
	})

	t.Run("does not retry other errors", func(t *testing.T) {
		t.Parallel()
		calls := 0
		err := renameRetry("a", "b", func(string, string) error {
			calls++
			return errOther
		}, retryable, policy)
		if !errors.Is(err, errOther) || calls != 1 {
			t.Fatalf("err = %v, calls = %d; want errOther, 1", err, calls)
		}
	})

	t.Run("gives up", func(t *testing.T) {
		t.Parallel()
		calls := 0
		err := renameRetry("a", "b", func(string, string) error {
			calls++
			return errBusy
		}, retryable, policy)
		// Waits of 1, 2, 4, 8 and 16ms fit in 50ms, so up to 6 calls are
		// made; slow timers (Windows) can fit fewer.
		if !errors.Is(err, errBusy) || calls < 2 || calls > 6 {
			t.Fatalf("err = %v, calls = %d; want errBusy, 2-6", err, calls)
		}
	})
}

// TestSetConfigWhileReading checks that SetConfig succeeds while another
// handle has config open for reading and is closed shortly after. On Windows
// the rename fails until that handle is closed, so this exercises the retry.
func TestSetConfigWhileReading(t *testing.T) {
	t.Parallel()
	r := initBareRepo(t)

	f, err := os.Open(filepath.Join(r.Path, "config"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(200 * time.Millisecond)
		f.Close() //nolint: errcheck,gosec
	}()
	defer func() { <-done }()

	cfg, err := r.Config()
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	cfg.Section("lfs").SetOption("url", "https://example.com/lfs")
	if err := r.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	got, err := r.Config()
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if u := got.Section("lfs").Option("url"); u != "https://example.com/lfs" {
		t.Fatalf("lfs.url = %q", u)
	}
}
