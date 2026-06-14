package racy

// This file contains four fundamental Go race condition patterns.
// Each test demonstrates a different class of race — from simple counter
// races to complex initialization races. These serve as the baseline
// educational examples in the racevis demo suite.
//
// See broker_test.go for realistic task-queue race conditions that
// mirror production bugs.

import (
	"os"
	"runtime/trace"
	"testing"
)

// TestMain initializes the runtime trace if RACEVIS_TRACE is set.
// Note: os.Exit skips defers, so trace.Stop() is called explicitly.
func TestMain(m *testing.M) {
	var traceFile *os.File
	if tf := os.Getenv("RACEVIS_TRACE"); tf != "" {
		f, err := os.Create(tf)
		if err == nil {
			trace.Start(f)
			traceFile = f
		}
	}

	code := m.Run()

	if traceFile != nil {
		trace.Stop()
		traceFile.Close()
	}

	os.Exit(code)
}

// TestCounterRace demonstrates a write/write race on an unprotected integer counter.
// Five goroutines all increment the same counter simultaneously.
// Fix: use sync/atomic or protect with sync.Mutex.
func TestCounterRace(t *testing.T) {
	_ = RunCounterRace()
}

// TestMapRace demonstrates concurrent read/write on a Go map.
// Go maps are not goroutine-safe — any concurrent write panics or races.
// Fix: protect with sync.RWMutex.
func TestMapRace(t *testing.T) {
	_ = RunMapRace()
}

// TestInitRace demonstrates a broken lazy singleton initialization.
// Multiple goroutines check-then-act on a nil pointer without proper sync.
// Fix: use sync.Once.
func TestInitRace(t *testing.T) {
	_ = RunInitRace()
}
