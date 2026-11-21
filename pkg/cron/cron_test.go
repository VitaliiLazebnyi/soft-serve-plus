package cron

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/robfig/cron/v3"
)

func TestCronLogger(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := log.New(&buf)
	logger.SetLevel(log.DebugLevel)
	clogger := cronLogger{logger}
	clogger.Info("foo")
	clogger.Error(fmt.Errorf("bar"), "test")
	if buf.String() != "DEBUG foo\nERROR test err=bar\n" {
		t.Errorf("unexpected log output: %s", buf.String())
	}
}

func TestSchedulerAddFunc(t *testing.T) {
	t.Parallel()
	s := NewScheduler(log.New(io.Discard))
	if err := s.AddFunc("test", "* * * * *", func() {}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddFunc("descriptor", "@every 1h", func() {}); err != nil {
		t.Fatal(err)
	}
	if n := len(s.cron.Entries()); n != 2 {
		t.Fatalf("%d entries, want 2", n)
	}
	if err := s.AddFunc("bad", "not a spec", func() {}); err == nil {
		t.Fatal("AddFunc accepted a bad spec")
	}
}

// onlyJob returns the wrapped job of the Scheduler's only entry.
func onlyJob(t *testing.T, s *Scheduler) cron.Job {
	t.Helper()
	entries := s.cron.Entries()
	if len(entries) != 1 {
		t.Fatalf("%d entries, want 1", len(entries))
	}
	return entries[0].WrappedJob
}

func TestSchedulerSkipsOverlappingRuns(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	s := NewScheduler(log.New(&logs))

	started := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32
	err := s.AddFunc("mirror-pull", "* * * * *", func() {
		runs.Add(1)
		started <- struct{}{}
		<-release
	})
	if err != nil {
		t.Fatal(err)
	}

	job := onlyJob(t, s)

	first := make(chan struct{})
	go func() {
		job.Run()
		close(first)
	}()
	<-started

	second := make(chan struct{})
	go func() {
		job.Run()
		close(second)
	}()

	select {
	case <-second:
	case <-started:
		t.Fatal("second run started while the first one was still running")
	case <-time.After(time.Minute):
		t.Fatal("second run did not return while the first one was running")
	}

	close(release)
	<-first

	if n := runs.Load(); n != 1 {
		t.Errorf("expected 1 run, got %d", n)
	}

	// The skip is logged with the job it skipped.
	if out := logs.String(); !strings.Contains(out, "skipping job run") || !strings.Contains(out, "job=mirror-pull") {
		t.Errorf("skip not logged with the job name: %q", out)
	}
}

// Shutdown waits for a running job to end, and gives up when ctx is done.
func TestSchedulerShutdownWaitsForRunningJob(t *testing.T) {
	t.Parallel()
	s := NewScheduler(log.New(io.Discard))
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	if err := s.AddFunc("slow", "@every 1s", func() {
		select {
		case started <- struct{}{}:
		default:
			return
		}
		<-release
	}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	select {
	case <-started:
	case <-time.After(time.Minute):
		t.Fatal("the job did not run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond) // meant to run out while the job runs
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown returned %v while the job ran, want %v", err, context.DeadlineExceeded)
	}

	close(release)
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown returned %v once the job ended, want nil", err)
	}
}

func TestSchedulerRecoversPanic(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := log.New(&buf)
	s := NewScheduler(logger)
	var runs atomic.Int32
	err := s.AddFunc("panics", "* * * * *", func() {
		if runs.Add(1) == 1 {
			panic("boom")
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	job := onlyJob(t, s)
	job.Run()

	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("expected the panic to be logged, got %q", buf.String())
	}

	// A panic must not leave the job marked as still running.
	job.Run()
	if n := runs.Load(); n != 2 {
		t.Errorf("expected the job to run again after a panic, got %d runs", n)
	}
}
