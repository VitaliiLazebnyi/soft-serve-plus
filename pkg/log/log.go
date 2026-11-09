package log

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
)

// NewLogger returns a new logger with default settings. If the configured
// log file cannot be opened, it returns the error together with a logger
// that writes to stderr, so the caller can keep logging.
func NewLogger(cfg *config.Config) (*log.Logger, *os.File, error) {
	if cfg == nil {
		return nil, nil, config.ErrNilConfig
	}
	logger := log.NewWithOptions(os.Stderr, log.Options{
		ReportTimestamp: true,
		TimeFormat:      time.DateOnly,
	})

	switch {
	case config.IsVerbose():
		logger.SetReportCaller(true)
		fallthrough
	case config.IsDebug():
		logger.SetLevel(log.DebugLevel)
	}

	logger.SetTimeFormat(cfg.Log.TimeFormat)

	switch strings.ToLower(cfg.Log.Format) {
	case "json":
		logger.SetFormatter(log.JSONFormatter)
	case "logfmt":
		logger.SetFormatter(log.LogfmtFormatter)
	case "text":
		logger.SetFormatter(log.TextFormatter)
	}

	var f *os.File
	if cfg.Log.Path != "" {
		// A relative log path resolves into the data directory, which may
		// not exist yet on a first run (the logger is created before
		// anything else touches the data path). Create the parent
		// directory private (0o700), the same mode the data path is
		// created with, so a first run does not leave the data directory
		// readable by other users.
		if err := os.MkdirAll(filepath.Dir(cfg.Log.Path), 0o700); err != nil {
			return nil, nil, err
		}
		var err error
		f, err = os.OpenFile(cfg.Log.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
		if err != nil {
			return logger, nil, err
		}
		logger.SetOutput(f)
	}

	return logger, f, nil
}
