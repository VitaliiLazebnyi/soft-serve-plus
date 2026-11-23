package git

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

func initBareRepo(t *testing.T) *Repository {
	t.Helper()
	r, err := Init(t.Context(), filepath.Join(t.TempDir(), "repo"), true)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	return r
}

func TestConfigRoundTrip(t *testing.T) {
	t.Parallel()
	r := initBareRepo(t)

	if _, ok, err := r.ConfigGet("lfs.url"); err != nil || ok {
		t.Fatalf("ConfigGet of an unset key = %v, %v; want unset", ok, err)
	}
	// Values a caller passes through: a leading dash, quotes, a
	// backslash, a newline, a comment character, surrounding spaces.
	for _, v := range []string{
		"https://example.com/repo.git",
		"-c core.sshCommand=evil",
		`a "quoted" \ value`,
		"two\nlines",
		"# not a comment ; nor this",
		"  padded  ",
		"",
	} {
		if err := r.ConfigSet("lfs.url", v); err != nil {
			t.Fatalf("ConfigSet(%q): %v", v, err)
		}
		got, ok, err := r.ConfigGet("lfs.url")
		if err != nil || !ok || got != v {
			t.Errorf("ConfigGet after setting %q = %q, %v, %v", v, got, ok, err)
		}
	}
	if _, err := os.Stat(filepath.Join(r.Path, "config.lock")); !os.IsNotExist(err) {
		t.Fatalf("config.lock left behind: %v", err)
	}
	// A key that starts with a dash is a key, not an option.
	if _, _, err := r.ConfigGet("--list"); err != nil {
		t.Errorf("ConfigGet(--list): %v", err)
	}
}

// ConfigSet replaces every value of a key; ConfigGet returns the last one,
// as git does; ConfigUnset removes them all and does not mind a key that
// is not set.
func TestConfigMultipleValues(t *testing.T) {
	t.Parallel()
	r := initBareRepo(t)
	cp := filepath.Join(r.Path, "config")
	f, err := os.OpenFile(cp, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[remote \"Up.Per\"]\n\turl = a\n\turl = b\n\tmirror\n[remote \"x\"]\n\turl = c\n[remote \"Up.Per\"]\n\turl = d\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, ok, err := r.ConfigGet("remote.Up.Per.url")
	if err != nil || !ok || got != "d" {
		t.Errorf("ConfigGet = %q, %v, %v; want the last value", got, ok, err)
	}
	entries, err := r.ConfigGetRegexp(`^remote\..*\.(url|mirror)$`)
	if err != nil {
		t.Fatalf("ConfigGetRegexp: %v", err)
	}
	want := []ConfigEntry{
		{"remote.Up.Per.url", "a"},
		{"remote.Up.Per.url", "b"},
		{"remote.Up.Per.mirror", ""},
		{"remote.x.url", "c"},
		{"remote.Up.Per.url", "d"},
	}
	if !slices.Equal(entries, want) {
		t.Errorf("ConfigGetRegexp = %q, want %q", entries, want)
	}
	if entries, err := r.ConfigGetRegexp(`^nothing\.`); err != nil || entries != nil {
		t.Errorf("ConfigGetRegexp with no match = %v, %v", entries, err)
	}

	if err := r.ConfigSet("remote.Up.Per.url", "e"); err != nil {
		t.Fatalf("ConfigSet: %v", err)
	}
	if entries, _ := r.ConfigGetRegexp(`^remote\.Up\.Per\.url$`); !slices.Equal(entries, []ConfigEntry{{"remote.Up.Per.url", "e"}}) {
		t.Errorf("after ConfigSet = %q, want the one value", entries)
	}
	if err := r.ConfigUnset("remote.Up.Per.url"); err != nil {
		t.Fatalf("ConfigUnset: %v", err)
	}
	if _, ok, err := r.ConfigGet("remote.Up.Per.url"); ok || err != nil {
		t.Errorf("after ConfigUnset: set %v, %v", ok, err)
	}
	if err := r.ConfigUnset("remote.Up.Per.url"); err != nil {
		t.Errorf("ConfigUnset of an unset key: %v", err)
	}
}

// TestConfigSetHonoursLock checks that ConfigSet, like git, refuses to write
// while config.lock is held and leaves both files as they were.
func TestConfigSetHonoursLock(t *testing.T) {
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

	if err := r.ConfigSet("lfs.url", "https://example.com/lfs"); err == nil {
		t.Fatal("ConfigSet succeeded while config.lock was held")
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

// TestConfigSetWhileReading checks that writes succeed while other git
// processes read the same file. On Windows a reader blocks the rename that
// replaces the file, and git retries it.
func TestConfigSetWhileReading(t *testing.T) {
	t.Parallel()
	r := initBareRepo(t)
	if err := r.ConfigSet("lfs.url", "start"); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, _, err := r.ConfigGet("lfs.url"); err != nil {
					t.Errorf("ConfigGet: %v", err)
					return
				}
			}
		})
	}
	for i := range 10 {
		if err := r.ConfigSet("lfs.url", "https://example.com/lfs"); err != nil {
			t.Errorf("ConfigSet %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	if got, _, err := r.ConfigGet("lfs.url"); err != nil || got != "https://example.com/lfs" {
		t.Fatalf("lfs.url = %q, %v", got, err)
	}
}
