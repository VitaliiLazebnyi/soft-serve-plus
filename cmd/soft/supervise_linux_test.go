package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// superviseHelperEnv names the scenario that TestSuperviseHelper runs. Each
// scenario runs in a re-executed test binary, so that the supervisor's wait
// for any child cannot collect processes the test framework started.
const superviseHelperEnv = "SOFT_TEST_SUPERVISE_SCENARIO"

const prSetChildSubreaper = 36 // PR_SET_CHILD_SUBREAPER from <linux/prctl.h>

// runSuperviseScenario runs scenario in a child test process that is a child
// subreaper, which makes orphans come to it as they would to PID 1. It returns
// the line the scenario printed. configure, if not nil, adjusts the command
// that starts the child test process.
func runSuperviseScenario(t *testing.T, scenario string, configure func(*exec.Cmd)) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSuperviseHelper$", "-test.v")
	cmd.Env = append(os.Environ(), superviseHelperEnv+"="+scenario)
	if configure != nil {
		configure(cmd)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper %s: %v\n%s", scenario, err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if r, ok := strings.CutPrefix(line, "RESULT "); ok {
			return r
		}
	}
	t.Fatalf("helper %s printed no result:\n%s", scenario, out)
	return ""
}

// TestSuperviseHelper is not a test of its own: it runs the scenario named in
// the environment, in the process that runSuperviseScenario started.
func TestSuperviseHelper(t *testing.T) {
	scenario := os.Getenv(superviseHelperEnv)
	if scenario == "" {
		t.Skip("helper for the supervise tests")
	}
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		t.Fatalf("prctl(PR_SET_CHILD_SUBREAPER): %v", errno)
	}
	sigs := make(chan os.Signal, 1)
	child := exec.CommandContext(t.Context(), "sh", "-c", "")
	var script, extra string
	var terminal chan string // the "terminal" scenario's findings
	switch scenario {
	case "orphans":
		// Each subshell starts a sleep and exits at once, so the sleep is
		// orphaned and re-parented to this process, as git's detached
		// maintenance is re-parented to PID 1. They exit while the child
		// still runs.
		script = `for i in 1 2 3; do (sleep 0.1 &); done; sleep 1; exit 3`
	case "signal":
		// SIGTERM is sent once the trap is set, which the script reports.
		script = `trap 'exit 7' TERM; echo ready; while :; do sleep 0.05; done`
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		child.Stdout = w
		go func() {
			line, _ := bufio.NewReader(r).ReadString('\n')
			if line == "ready\n" {
				sigs <- syscall.SIGTERM
			}
		}()
	case "killed":
		script = `kill -KILL $$`
	case "pgroup":
		// Field 5 of /proc/PID/stat is the process group.
		out := filepath.Join(t.TempDir(), "pgid")
		script = `cut -d' ' -f5 /proc/$$/stat >` + out
		defer func() {
			b, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			pgid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			fmt.Printf("RESULT samegroup=%v\n", pgid == syscall.Getpgrp())
		}()
	case "terminal":
		// The script prints its process group and waits for SIGTERM,
		// which is sent once the terminal's foreground group, or the
		// error asking for it, has been noted.
		script = `trap 'exit 7' TERM; cut -d' ' -f5 /proc/$$/stat; while :; do sleep 0.05; done`
		child.Stdin = os.Stdin
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		child.Stdout = w
		terminal = make(chan string, 1)
		go func() {
			line, _ := bufio.NewReader(r).ReadString('\n')
			pgid, _ := strconv.Atoi(strings.TrimSpace(line))
			foreground := ""
			if tpgid, err := ioctlGetInt(os.Stdin, syscall.TIOCGPGRP); err != nil {
				foreground = err.Error()
			} else {
				foreground = strconv.FormatBool(tpgid == pgid)
			}
			terminal <- fmt.Sprintf("owngroup=%v foreground=%s", pgid != syscall.Getpgrp(), foreground)
			sigs <- syscall.SIGTERM
		}()
	default:
		t.Fatalf("unknown scenario %q", scenario)
	}
	child.Args[2] = script
	code, err := supervise(child, sigs)
	if err != nil {
		t.Fatal(err)
	}
	extra = fmt.Sprintf("code=%d zombies=%d", code, zombieChildren(t))
	if terminal != nil {
		extra += " " + <-terminal
	}
	if scenario != "pgroup" {
		fmt.Println("RESULT " + extra)
	}
}

// zombieChildren counts the zombie processes whose parent is this process.
func zombieChildren(t *testing.T) int {
	t.Helper()
	stats, err := filepath.Glob("/proc/[0-9]*/stat")
	if err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	n := 0
	for _, path := range stats {
		b, err := os.ReadFile(path)
		if err != nil {
			continue // the process exited meanwhile
		}
		// "pid (comm) state ppid ...": comm may contain spaces and
		// parentheses, so split after the last ')'.
		s := string(b)
		fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(fields) < 2 {
			continue
		}
		if ppid, _ := strconv.Atoi(fields[1]); fields[0] == "Z" && ppid == self {
			n++
		}
	}
	return n
}

func TestSuperviseReapsOrphans(t *testing.T) {
	t.Parallel()
	if got, want := runSuperviseScenario(t, "orphans", nil), "code=3 zombies=0"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSuperviseForwardsSignals(t *testing.T) {
	t.Parallel()
	if got, want := runSuperviseScenario(t, "signal", nil), "code=7 zombies=0"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSuperviseReportsDeathBySignal(t *testing.T) {
	t.Parallel()
	if got, want := runSuperviseScenario(t, "killed", nil), "code=137 zombies=0"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestSuperviseChildHasOwnGroup checks that the child gets a process group of
// its own, so a terminal's ^C, sent to the supervisor's group, does not reach
// the server a second time besides the forwarded copy.
func TestSuperviseChildHasOwnGroup(t *testing.T) {
	t.Parallel()
	if got, want := runSuperviseScenario(t, "pgroup", nil), "samegroup=false"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ioctlGetInt runs on f the ioctl req, which stores an int.
func ioctlGetInt(f *os.File, req uintptr) (int, error) {
	var v int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(unsafe.Pointer(&v))); errno != 0 {
		return 0, errno
	}
	return int(v), nil
}

// openPty opens a pseudo-terminal and returns its secondary end; the primary
// end stays open until the test ends.
func openPty(t *testing.T) *os.File {
	t.Helper()
	primary, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminals: %v", err)
	}
	t.Cleanup(func() { _ = primary.Close() })
	var unlock int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, primary.Fd(), syscall.TIOCSPTLCK,
		uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		t.Fatalf("unlockpt: %v", errno)
	}
	n, err := ioctlGetInt(primary, syscall.TIOCGPTN)
	if err != nil {
		t.Fatalf("ptsname: %v", err)
	}
	tty, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tty.Close() })
	return tty
}

// TestSuperviseChildOwnsTerminal checks that, when stdin is the supervisor's
// controlling terminal, the child's group becomes the terminal's foreground
// group, so that the terminal signals only the child and lets it read.
func TestSuperviseChildOwnsTerminal(t *testing.T) {
	t.Parallel()
	tty := openPty(t)
	got := runSuperviseScenario(t, "terminal", func(cmd *exec.Cmd) {
		// The helper leads a session whose controlling terminal is the
		// pty, as soft does in a container started with docker run -t.
		cmd.Stdin = tty
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	})
	if want := "code=7 zombies=0 owngroup=true foreground=true"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestSuperviseTerminalNotControlling checks that the server is still
// supervised, in a group of its own, when stdin is a terminal that is not the
// supervisor's controlling terminal and so cannot be handed to the child.
func TestSuperviseTerminalNotControlling(t *testing.T) {
	t.Parallel()
	tty := openPty(t)
	got := runSuperviseScenario(t, "terminal", func(cmd *exec.Cmd) {
		// The helper leads a session without a controlling terminal.
		cmd.Stdin = tty
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	})
	if want := "code=7 zombies=0 owngroup=true foreground=" + syscall.ENOTTY.Error(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
