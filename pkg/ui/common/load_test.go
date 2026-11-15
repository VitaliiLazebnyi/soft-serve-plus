package common

import (
	"context"
	"errors"
	"testing"
)

// Starting a load cancels the one before it; Stop cancels the one in
// flight.
func TestLoader(t *testing.T) {
	var l Loader
	first := l.Start(context.Background())
	second := l.Start(context.Background())
	if first.Err() == nil {
		t.Fatal("a new load did not cancel the one before")
	}
	if second.Err() != nil {
		t.Fatal("the new load is cancelled")
	}
	l.Stop()
	if second.Err() == nil {
		t.Fatal("Stop did not cancel the load in flight")
	}
	l.Stop()

	parent, cancel := context.WithCancel(context.Background())
	third := l.Start(parent)
	cancel()
	if third.Err() == nil {
		t.Fatal("a load outlived its parent context")
	}
}

// A cancelled load reports nothing; any other failure is an error.
func TestLoadError(t *testing.T) {
	err := errors.New("boom")
	if msg := LoadError(context.Background(), err); msg == nil {
		t.Fatal("a failed load reported nothing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if msg := LoadError(ctx, err); msg != nil {
		t.Fatalf("a cancelled load reported %v", msg)
	}
}
