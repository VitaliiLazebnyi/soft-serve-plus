package sync

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestWorkPool(t *testing.T) {
	mtx := &sync.Mutex{}
	values := make([]int, 0)
	wp := NewWorkPool(context.Background(), 3)
	for i := 0; i < 10; i++ {
		id := strconv.Itoa(i)
		i := i
		wp.Add(id, func() {
			mtx.Lock()
			values = append(values, i)
			mtx.Unlock()
		})
	}
	wp.Run()

	if len(values) != 10 {
		t.Errorf("expected 10 values, got %d, %v", len(values), values)
	}

	for i := range values {
		id := strconv.Itoa(i)
		if wp.Status(id) {
			t.Errorf("expected %s to be false", id)
		}
	}
}

// TestWorkPoolRecoversPanic checks that a panicking job is logged and
// removed without killing the process or stopping the other jobs.
func TestWorkPoolRecoversPanic(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	ran := false
	wp := NewWorkPool(context.Background(), 1, WithWorkPoolLogger(func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}))
	wp.Add("panics", func() { panic("boom") })
	wp.Add("runs", func() {
		mu.Lock()
		defer mu.Unlock()
		ran = true
	})
	wp.Run()

	if !ran {
		t.Error("expected the other job to run")
	}
	if wp.Status("panics") {
		t.Error("expected the panicking job to be removed from the pool")
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "job panics panicked: boom") {
		t.Errorf("expected the panic to be logged, got %q", logs)
	}
}
