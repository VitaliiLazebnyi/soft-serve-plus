package hooks

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// PushRefsEnv names the file a push's post-receive hook writes the refs it
// was given to, one "<old> <new> <ref>" line each, as git gives them. The
// server sets it for receive-pack (see PushRefs) and reads the file once
// receive-pack has exited.
const PushRefsEnv = "SOFT_SERVE_PUSH_REFS"

// maxPushRefsSize bounds how much of a push's refs file is read. A ref line
// is at most a few hundred bytes, so this is far more than any real push
// and only keeps a broken hook from making the server read without end.
const maxPushRefsSize = 64 << 20

// PushRefs is the file that collects the refs one push updated. git runs
// post-receive after it has updated the refs, with only those it updated,
// so this is exactly what the push changed, whatever other pushes to the
// same repository do meanwhile.
type PushRefs struct {
	path string
}

// NewPushRefs creates an empty refs file for one push, readable and writable
// only by the server's user. The caller must call Remove.
func NewPushRefs() (*PushRefs, error) {
	f, err := os.CreateTemp("", "soft-serve-push-*.refs")
	if err != nil {
		return nil, fmt.Errorf("create push refs file: %w", err)
	}
	// The hook, another process, appends to it; it is read again after
	// receive-pack has exited. Windows would refuse to remove it later
	// while it is open.
	if err := f.Close(); err != nil {
		os.Remove(f.Name()) //nolint: errcheck
		return nil, fmt.Errorf("create push refs file: %w", err)
	}
	return &PushRefs{path: f.Name()}, nil
}

// Env returns the variable that tells the post-receive hook where to write.
func (p *PushRefs) Env() string {
	return PushRefsEnv + "=" + p.path
}

// Refs returns the refs the hook wrote, in the order git gave them. Lines
// that are not three fields with object IDs are left out and returned in
// the error, joined with any read error, so the caller can log them.
func (p *PushRefs) Refs() ([]HookArg, error) {
	f, err := os.Open(p.path)
	if err != nil {
		return nil, fmt.Errorf("open push refs file: %w", err)
	}
	defer f.Close() //nolint: errcheck
	return parseRefs(io.LimitReader(f, maxPushRefsSize))
}

// Remove removes the refs file.
func (p *PushRefs) Remove() error {
	if err := os.Remove(p.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove push refs file: %w", err)
	}
	return nil
}

// errBadRefLine is a line of a refs file that is not "<old> <new> <ref>".
var errBadRefLine = errors.New("invalid ref line")

// parseRefs reads "<old> <new> <ref>" lines.
func parseRefs(r io.Reader) ([]HookArg, error) {
	var refs []HookArg
	var errs []error
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || !isObjectID(fields[0]) || !isObjectID(fields[1]) {
			errs = append(errs, fmt.Errorf("%w: %q", errBadRefLine, line))
			continue
		}
		refs = append(refs, HookArg{OldSha: fields[0], NewSha: fields[1], RefName: fields[2]})
	}
	if err := scanner.Err(); err != nil {
		errs = append(errs, fmt.Errorf("read push refs file: %w", err))
	}
	return refs, errors.Join(errs...)
}

// isObjectID reports whether s is a SHA-1 or SHA-256 object ID in hex.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
