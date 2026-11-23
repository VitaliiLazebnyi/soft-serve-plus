package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// The repository's own config file is read and written with `git config
// --file`, which takes the same lock git does and replaces the file by
// renaming, retrying on Windows while another process has it open. Like
// --local it reads that one file, without its includes.
// runConfig runs `git config --file <repo>/config` with args under ctx. It
// returns git's output and its exit code, or an error when git did not run
// or exited with a code not in ok.
func (r *Repository) runConfig(ctx context.Context, ok []int, args ...string) ([]byte, int, error) {
	args = append([]string{"config", "--file", filepath.Join(r.Path, "config")}, args...)
	var out, stderr bytes.Buffer
	err := NewCommand(ctx, args...).RunInDirPipeline(&out, &stderr, r.Path)
	if err == nil {
		return out.Bytes(), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		for _, code := range ok {
			if exitErr.ExitCode() == code {
				return out.Bytes(), code, nil
			}
		}
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return nil, 0, fmt.Errorf("git config: %w - %s", err, msg)
	}
	return nil, 0, fmt.Errorf("git config: %w", err)
}

// ConfigGet returns the last value of key in the repository's config file,
// as git does, and whether key is set there.
func (r *Repository) ConfigGet(key string) (string, bool, error) {
	// Exit code 1: the key is not set.
	out, code, err := r.runConfig(r.ctx, []int{1}, "--null", "--get", "--end-of-options", key)
	if err != nil || code == 1 {
		return "", false, err
	}
	return strings.TrimSuffix(string(out), "\x00"), true, nil
}

// ConfigGetRegexp returns every value in the repository's config file whose
// key matches the regular expression keyRegexp, in file order. Keys are as
// git prints them: the section and the name in lower case, a subsection as
// written, for example remote.Origin.url.
func (r *Repository) ConfigGetRegexp(keyRegexp string) ([]ConfigEntry, error) {
	// Exit code 1: nothing matches.
	out, code, err := r.runConfig(r.ctx, []int{1}, "--null", "--get-regexp", "--end-of-options", keyRegexp)
	if err != nil || code == 1 {
		return nil, err
	}
	var entries []ConfigEntry
	for rec := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		// A key without a value, which reads as true, has no newline.
		key, value, _ := strings.Cut(rec, "\n")
		entries = append(entries, ConfigEntry{Key: key, Value: value})
	}
	return entries, nil
}

// writeContext is the context a config write runs under: r's values
// without its cancellation. git writes the file through config.lock, and a
// git killed mid-write would leave the lock behind and refuse every later
// write, so a write finishes once started.
func (r *Repository) writeContext() context.Context {
	return context.WithoutCancel(r.ctx)
}

// ConfigSet sets key to value in the repository's config file, replacing
// every value it had. See writeContext.
func (r *Repository) ConfigSet(key, value string) error {
	_, _, err := r.runConfig(r.writeContext(), nil, "--replace-all", "--end-of-options", key, value)
	return err
}

// ConfigUnset removes every value of key from the repository's config file.
// A key that is not set is not an error. See writeContext.
func (r *Repository) ConfigUnset(key string) error {
	// Exit code 5: the key is not set.
	_, _, err := r.runConfig(r.writeContext(), []int{5}, "--unset-all", "--end-of-options", key)
	return err
}
