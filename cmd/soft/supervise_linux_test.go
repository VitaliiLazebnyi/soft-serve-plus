package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// superviseHelperEnv names the scenario that TestSuperviseHelper runs. Each
// scenario runs in a re-executed test binary, so that the supervisor's wait
// for any child cannot collect processes the test framework started.
const superviseHelperEnv = "SOFT_TEST_SUPERVISE_SCENARIO"

const prSetChildSubreaper = 36 // PR_SET_CHILD_SUBREAPER from <linux/prctl.h>

// runSuperviseScenario runs scenario in a child test process that is a child
// subreaper, which makes orphans come to it as they would to PID 1. It returns
// the line the scenario printed.
func runSuperviseScenario(t *testing.T, scenario string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSuperviseHelper$", "-test.v")
	cmd.Env = append(os.Environ(), superviseHelperEnv+"="+scenario)
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
	var script string
	switch scenario {
	case "orphans":
		// Each subshell starts a sleep and exits at once, so the sleep is
		// orphaned and re-parented to this process, as git's detached
		// maintenance is re-parented to PID 1. They exit while the child
		// still runs.
		script = `for i in 1 2 3; do (sleep 0.1 &); done; sleep 1; exit 3`
	case "signal":
		script = `trap 'exit 7' TERM; while :; do sleep 0.05; done`
		go func() {
			time.Sleep(300 * time.Millisecond)
			sigs <- syscall.SIGTERM
		}()
	case "killed":
		script = `kill -KILL $$`
	default:
		t.Fatalf("unknown scenario %q", scenario)
	}
	code, err := supervise(exec.CommandContext(t.Context(), "sh", "-c", script), sigs)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("RESULT code=%d zombies=%d\n", code, zombieChildren(t))
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
	if got, want := runSuperviseScenario(t, "orphans"), "code=3 zombies=0"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSuperviseForwardsSignals(t *testing.T) {
	t.Parallel()
	if got, want := runSuperviseScenario(t, "signal"), "code=7 zombies=0"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSuperviseReportsDeathBySignal(t *testing.T) {
	t.Parallel()
	if got, want := runSuperviseScenario(t, "killed"), "code=137 zombies=0"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
