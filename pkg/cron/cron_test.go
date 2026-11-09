package cron

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/robfig/cron/v3"
)

func TestCronLogger(t *testing.T) {
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

func TestSchedularAddRemove(t *testing.T) {
	s := NewScheduler(context.TODO())
	id, err := s.AddFunc("test", "* * * * *", func() {})
	if err != nil {
		t.Fatal(err)
	}
	s.Remove(id)
}

func TestSchedulerSkipsOverlappingRuns(t *testing.T) {
	var logs bytes.Buffer
	s := NewScheduler(log.WithContext(context.TODO(), log.New(&logs)))

	started := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32
	id, err := s.AddFunc("mirror-pull", "* * * * *", func() {
		runs.Add(1)
		started <- struct{}{}
		<-release
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Remove(id)

	job := s.Cron.Entry(cron.EntryID(id)).WrappedJob

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
	case <-time.After(5 * time.Second):
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

func TestSchedulerRecoversPanic(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf)
	s := NewScheduler(log.WithContext(context.TODO(), logger))
	var runs atomic.Int32
	id, err := s.AddFunc("panics", "* * * * *", func() {
		if runs.Add(1) == 1 {
			panic("boom")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Remove(id)

	job := s.Cron.Entry(cron.EntryID(id)).WrappedJob
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
