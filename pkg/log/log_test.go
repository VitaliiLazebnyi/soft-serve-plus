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

func TestNewLoggerCreatesMissingLogDir(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), "does", "not", "exist")
	cfg := config.DefaultConfig()
	cfg.DataPath = dataPath
	cfg.Log.Path = filepath.Join("logs", "soft.log")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	want := filepath.Join(dataPath, "logs", "soft.log")
	if cfg.Log.Path != want {
		t.Fatalf("expected log path %q, got %q", want, cfg.Log.Path)
	}

	logger, f, err := NewLogger(cfg)
	if err != nil {
		t.Fatalf("expected nil got %v", err)
	}
	if logger == nil || f == nil {
		t.Fatalf("expected logger and file, got %v %v", logger, f)
	}
	t.Cleanup(func() { _ = f.Close() })
	logger.Info("hello")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected log file to exist: %v", err)
	}
	if runtime.GOOS == "windows" {
		return // unix permissions
	}
	for _, dir := range []string{dataPath, filepath.Dir(want)} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o700 {
			t.Errorf("%s mode = %v, want 0700", dir, got)
		}
	}
}

func TestNewLoggerFallsBackToStderr(t *testing.T) {
	c := &config.Config{Log: config.LogConfig{Path: filepath.Join(t.TempDir(), "missing", "logfile.txt")}}
	logger, f, err := NewLogger(c)
	if err == nil {
		t.Fatal("expected error got nil")
	}
	if f != nil {
		t.Errorf("expected no file got %v", f.Name())
	}
	if logger == nil {
		t.Fatal("expected a stderr logger got nil")
	}
	logger.Info("still logging")
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
