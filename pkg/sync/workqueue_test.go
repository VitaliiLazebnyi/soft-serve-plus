package sync

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// Run waits for the work already running when the pool's context is
// cancelled, and starts no more.
func TestWorkPoolRunWaitsAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wp := NewWorkPool(ctx, 1)
	var finished, ran atomic.Int32
	for i := 0; i < 2; i++ {
		wp.Add(strconv.Itoa(i), func() {
			if ran.Add(1) > 1 {
				return
			}
			cancel()
			time.Sleep(200 * time.Millisecond)
			finished.Add(1)
		})
	}
	wp.Run()
	if finished.Load() != 1 {
		t.Fatal("Run returned before the running work ended")
	}
	if n := ran.Load(); n != 1 {
		t.Fatalf("%d pieces of work ran, want 1", n)
	}
}
