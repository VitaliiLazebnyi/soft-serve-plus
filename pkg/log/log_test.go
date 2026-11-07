package log

import (
	"os"
	"path/filepath"
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
}
