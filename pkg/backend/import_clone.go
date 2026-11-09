package backend

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// cloneWaitDelay is how long a clone stopped by its context may take to
// release its output once git has been killed. A transport helper git
// started (ssh, git-remote-https) may outlive it holding the pipes, and
// without the delay the wait would last as long as the helper does.
const cloneWaitDelay = 5 * time.Second

// cloneImport clones imp.remote into imp.path as a bare repository, a
// mirror when asked for. ctx stops the clone: git and the transport helpers
// it started are killed (on Windows, git only), so a remote that stalls
// cannot keep them running once the import is given up.
func cloneImport(ctx context.Context, imp importSpec) error {
	args := []string{"clone", "--bare", "--quiet"}
	if imp.opts.Mirror {
		args = append(args, "--mirror")
	}
	args = append(args, "--end-of-options", imp.remote, imp.path)

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), imp.env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	killTreeOnCancel(cmd)
	cmd.WaitDelay = cloneWaitDelay

	if err := cmd.Run(); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			err = fmt.Errorf("%w: %w", cerr, err)
		}
		if msg := strings.TrimSpace(out.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}
