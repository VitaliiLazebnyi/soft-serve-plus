package log

import (
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
