// Package safego runs functions so that a panic in them is logged instead of
// ending the process.
//
// A panic in a goroutine is not recovered by anything up its caller's stack:
// the server's per-request and per-session recovers only cover the
// goroutines they run in, so a panic in a goroutine a handler starts ends the
// whole server. Goroutines that handle repository or network data use Go.
package safego

import (
	"fmt"
	"runtime/debug"

	"charm.land/log/v2"
)

// PanicError is a panic recovered by Run.
type PanicError struct {
	// Value is the value passed to panic.
	Value any
	// Stack is the stack of the goroutine that panicked.
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("panic: %v", e.Value)
}

// Unwrap returns the panic value when it is an error.
func (e *PanicError) Unwrap() error {
	err, _ := e.Value.(error)
	return err
}

// Run calls fn. If fn panics, Run recovers, logs the panic and its stack to
// logger (the default logger if nil) at Error, and returns it as a
// *PanicError; otherwise it returns nil. Deferred calls in fn have run by
// then, as for any panic.
func Run(logger *log.Logger, fn func()) (err error) {
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		perr := &PanicError{Value: v, Stack: debug.Stack()}
		if logger == nil {
			logger = log.Default()
		}
		logger.Error("recovered panic", "panic", v, "stack", string(perr.Stack))
		err = perr
	}()
	fn()
	return nil
}

// Go runs fn in a new goroutine through Run, so a panic in it is logged
// rather than ending the process.
func Go(logger *log.Logger, fn func()) {
	go Run(logger, fn) //nolint: errcheck // Run has logged the panic.
}
