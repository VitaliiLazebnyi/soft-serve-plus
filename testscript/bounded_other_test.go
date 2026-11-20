//go:build !windows

package testscript

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// interruptSignals are the signals that stop runBounded early.
var interruptSignals = []os.Signal{os.Interrupt, syscall.SIGQUIT, syscall.SIGTERM}

// ownProcessGroup starts cmd in a process group of its own, which the
// processes it starts, such as git's ssh, join, so that they can be listed
// and killed together.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree kills p's process group (see ownProcessGroup).
func killProcessTree(p *os.Process) {
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
}

// quitProcess sends p SIGQUIT, on which a Go program prints the stacks of
// its goroutines and exits.
func quitProcess(p *os.Process) error {
	return p.Signal(syscall.SIGQUIT)
}

// describeProcesses writes the processes of process group pgid to w, and
// with withFiles their open files too.
func describeProcesses(w io.Writer, pgid int, withFiles bool) {
	lines := strings.Split(runDiagnostic("ps", "-A", "-o", "pid,ppid,pgid,stat,etime,command"), "\n")
	_, _ = fmt.Fprintf(w, "harness: processes in group %d:\n", pgid)
	for i, line := range lines {
		if f := strings.Fields(line); i == 0 || len(f) > 2 && f[2] == strconv.Itoa(pgid) {
			_, _ = fmt.Fprintln(w, line)
		}
	}
	if withFiles {
		_, _ = fmt.Fprintf(w, "harness: their open files:\n%s", runDiagnostic("lsof", "-nP", "-g", strconv.Itoa(pgid)))
	}
}
