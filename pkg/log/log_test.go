package log

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

func TestGoodNewLogger(t *testing.T) {
	for _, c := range []*config.Config{
		config.DefaultConfig(),
		{},
		{Log: config.LogConfig{Path: filepath.Join(t.TempDir(), "logfile.txt")}},
	} {
		_, f, err := NewLogger(c)
		if err != nil {
			t.Errorf("expected nil got %v", err)
		}
		if f != nil {
			if err := f.Close(); err != nil {
				t.Errorf("failed to close logger: %v", err)
			}
		}
	}
}

func TestBadNewLogger(t *testing.T) {
	for _, c := range []*config.Config{
		nil,
		{Log: config.LogConfig{Path: "\x00"}},
	} {
		_, f, err := NewLogger(c)
		if err == nil {
			t.Errorf("expected error got nil")
		}
		if f != nil {
			if err := f.Close(); err != nil {
				t.Errorf("failed to close logger: %v", err)
			}
		}
	}
}

// The log file is not readable by other users.
func TestNewLoggerFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	path := filepath.Join(t.TempDir(), "soft-serve.log")
	_, f, err := NewLogger(&config.Config{Log: config.LogConfig{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got&0o007 != 0 || got&0o020 != 0 {
		t.Errorf("log file mode = %v, want no group write or other access", got)
	}
}
