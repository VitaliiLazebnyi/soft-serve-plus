package jobs

import (
	"context"
	"testing"
)

type nopRunner struct{}

func (nopRunner) Spec(context.Context) string { return "@every 1h" }
func (nopRunner) Func(context.Context) func() { return func() {} }

// List hands out a copy: changing it does not change what is registered.
func TestListReturnsCopy(t *testing.T) {
	const name = "test-list-copy"
	Register(name, nopRunner{})
	t.Cleanup(func() {
		mtx.Lock()
		delete(jobs, name)
		mtx.Unlock()
	})

	l := List()
	j, ok := l[name]
	if !ok {
		t.Fatalf("%s not listed", name)
	}
	delete(l, name)
	l["test-list-copy-added"] = &Job{Runner: nopRunner{}}

	again := List()
	if _, ok := again[name]; !ok {
		t.Errorf("deleting from the listed map unregistered %s", name)
	}
	if _, ok := again["test-list-copy-added"]; ok {
		t.Error("adding to the listed map registered a job")
	}

	// The jobs are shared, so the scheduler's ID is kept with the job.
	j.ID = 42
	if got := List()[name].ID; got != 42 {
		t.Errorf("job ID = %d, want 42", got)
	}
}
