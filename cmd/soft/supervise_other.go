//go:build !unix

package main

// superviseIfInit does nothing: only Unix systems have a PID 1 that must reap
// orphaned processes.
func superviseIfInit() {}
