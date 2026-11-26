//go:build linux

package gitserve

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/git"
)

// prSetChildSubreaper is PR_SET_CHILD_SUBREAPER from <linux/prctl.h>.
const prSetChildSubreaper = 36

// TestReceivePackMaintenanceDoesNotDetach pushes a commit, which makes
// receive-pack start automatic maintenance, and checks that no maintenance
// process outlives the push.
//
// Since git 2.47 a detached "git maintenance run --auto" daemonizes after
// every push that updates a ref, whether or not it has work. Its parent
// exits, so it is re-parented to the nearest subreaper, normally PID 1. With
// the server as PID 1 (a container without an init) nothing reaps it and
// each push leaves a zombie. The test makes itself a child subreaper, which
// puts it where PID 1 would be: any process it finds as its child after the
// handler returned is such an orphan, since os/exec waits for the children
// it started.
func TestReceivePackMaintenanceDoesNotDetach(t *testing.T) {
	// Not parallel: the subreaper setting is process-wide, and the
	// operator case sets the process environment.
	if v, err := git.BinVersion(t.Context()); err != nil || v.Less(git.Version{Major: 2, Minor: 47}) {
		t.Skipf("git %v (%v) does not detach maintenance on every push", v, err)
	}
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		t.Skipf("PR_SET_CHILD_SUBREAPER: %v", errno)
	}
	t.Cleanup(func() {
		syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 0, 0) //nolint: errcheck
	})

	isolated := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}

	tests := []struct {
		name string
		// repoConfig is set in the repository's own config.
		repoConfig [][2]string
		// operatorEnv is GIT_CONFIG_* in the server's environment.
		operatorEnv [][2]string
		wantOrphan  bool
	}{
		{name: "default"},
		{
			// The environment overrides configuration files.
			name:       "repository config asks to detach",
			repoConfig: [][2]string{{"gc.autoDetach", "true"}, {"maintenance.autoDetach", "true"}},
		},
		{
			// An operator can still opt back in through the
			// environment; this also shows the test sees a detach.
			name:        "operator environment asks to detach",
			operatorEnv: [][2]string{{"maintenance.autoDetach", "true"}},
			wantOrphan:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.operatorEnv) > 0 {
				t.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(len(tc.operatorEnv)))
				for i, kv := range tc.operatorEnv {
					t.Setenv("GIT_CONFIG_KEY_"+strconv.Itoa(i), kv[0])
					t.Setenv("GIT_CONFIG_VALUE_"+strconv.Itoa(i), kv[1])
				}
			}
			pack, commit := packWithBlob(t, 1024)
			dir := initBare(t)
			for _, kv := range tc.repoConfig {
				gitRun(t, dir, nil, "config", kv[0], kv[1])
			}
			// The setup's own "git commit" starts detached maintenance
			// too; reap those orphans first.
			orphans(t)

			var in bytes.Buffer
			line := fmt.Sprintf("%s %s refs/heads/main\x00report-status\n", strings.Repeat("0", 40), commit)
			fmt.Fprintf(&in, "%04x%s0000", len(line)+4, line)
			in.Write(pack)
			var out, stderr bytes.Buffer
			ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
			defer cancel()
			err := ReceivePackService.Handler(ctx, ServiceCommand{
				Stdin:  &in,
				Stdout: &out,
				Stderr: &stderr,
				Dir:    dir,
				Env:    isolated,
			})
			if err != nil {
				t.Fatalf("receive-pack: %v\n%s", err, stderr.String())
			}
			if !strings.Contains(out.String(), "ok refs/heads/main") {
				t.Fatalf("push not accepted: %q\n%s", out.String(), stderr.String())
			}

			// A detached process is re-parented before receive-pack
			// exits; give a slow one time all the same. Where one is
			// wanted, wait for it up to a minute. Where none is, a second
			// is a check that none appeared, which load can only make
			// pass.
			wait := time.Second
			if tc.wantOrphan {
				wait = time.Minute
			}
			var found []string
			for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				if found = orphans(t); len(found) > 0 {
					break
				}
			}
			switch {
			case tc.wantOrphan && len(found) == 0:
				t.Error("no detached maintenance with the operator's maintenance.autoDetach=true")
			case !tc.wantOrphan && len(found) > 0:
				t.Errorf("push left detached processes behind: %v", found)
			}
		})
	}
}

// orphans returns the processes, other than those os/exec is waiting for,
// whose parent is this process, and reaps them. Each is described by its
// PID, state and command line (empty once it is a zombie).
func orphans(t *testing.T) []string {
	t.Helper()
	self := os.Getpid()
	stats, err := filepath.Glob("/proc/[0-9]*/stat")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, p := range stats {
		b, err := os.ReadFile(p)
		if err != nil {
			continue // gone
		}
		// "pid (comm) state ppid ...": comm may hold spaces and parens.
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 2 || f[1] != strconv.Itoa(self) {
			continue
		}
		pid, err := strconv.Atoi(filepath.Base(filepath.Dir(p)))
		if err != nil {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join(filepath.Dir(p), "cmdline"))
		found = append(found, fmt.Sprintf("%d %s %q", pid, f[0], strings.ReplaceAll(string(cmdline), "\x00", " ")))
		reap(t, pid)
	}
	return found
}

// reap waits for the orphan pid, so it does not stay a zombie for the rest
// of the test binary.
func reap(t *testing.T, pid int) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ws syscall.WaitStatus
		syscall.Wait4(pid, &ws, 0, nil) //nolint: errcheck
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Errorf("orphan %d did not exit", pid)
	}
}
