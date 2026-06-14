// Package runner executes go test against a target Go package and collects
// two kinds of output:
//
//  1. Race detector output — from go test -race (which goroutines touched
//     the same memory address concurrently)
//
//  2. Scheduler trace — from go test -trace (when each goroutine ran,
//     blocked, and was created)
//
// These run as separate passes because -race and -trace conflict on Go 1.23+.
// See ADR-001 for the full reasoning.
package runner

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Result holds the raw outputs from both test passes.
type Result struct {
	// RaceOutput is the combined stdout+stderr from go test -race.
	// Contains "WARNING: DATA RACE" blocks when races are found.
	RaceOutput []byte

	// TraceFile is the path to the runtime trace file written by pass 2.
	// Empty string means trace collection failed or produced no output.
	TraceFile string

	// ExitCode from go test -race. 0 = pass, 1 = races found, 2 = build error.
	ExitCode int

	// NoTests is true when the target package has no _test.go files.
	// racevis cannot detect races without tests — the caller should warn the user.
	NoTests bool
}

// RunTarget runs two passes against the given Go package directory:
//
//   - Pass 1: go test -race ./...  → captures race detector output
//   - Pass 2: go test -trace=file . → captures scheduler trace (root package only)
//
// traceFilePath is the file path for the runtime trace output.
// count controls how many times pass 1 runs — higher values catch more intermittent races.
// runFilter is passed as -run to go test — empty string runs all tests.
// Use os.CreateTemp to get a suitable path for traceFilePath.
func RunTarget(pkgPath string, traceFilePath string, count int, runFilter string) (*Result, error) {
	if count < 1 {
		count = 1
	}
	// Validate inputs before touching the filesystem
	if err := validateTarget(pkgPath); err != nil {
		return nil, err
	}

	abs, err := filepath.Abs(pkgPath)
	if err != nil {
		return nil, fmt.Errorf("runner: resolving path %s: %w", pkgPath, err)
	}

	// Pass 1: race detection across all sub-packages.
	// raceBytes holds the raw race detector text output (not to be confused
	// with traceFilePath which is the scheduler trace file path).
	log.Printf("[runner] pass 1: go test -race ./... in %s", abs)
	// Run pass 1 `count` times, merging all race output.
	// Multiple runs catch races that only manifest under specific scheduling.
	var allRaceBytes []byte
	exitCode := 0
	for i := 0; i < count; i++ {
		if count > 1 {
			log.Printf("[runner] pass 1 run %d/%d", i+1, count)
		}
		args := []string{"test", "-race", "-count=1", "-timeout=60s"}
		if runFilter != "" {
			args = append(args, "-run", runFilter)
		}
		args = append(args, "./...")
		raceBytes, code, err := runGoTest(abs, args)
		if err != nil {
			return nil, err
		}
		allRaceBytes = append(allRaceBytes, raceBytes...)
		if code > exitCode {
			exitCode = code
		}
	}
	raceBytes := allRaceBytes
	if err != nil {
		return nil, err
	}

	result := &Result{
		RaceOutput: allRaceBytes,
		ExitCode:   exitCode,
	}

	// Detect "no test files" — racevis needs tests to find races
	if bytes.Contains(raceBytes, []byte("[no test files]")) && exitCode == 0 {
		result.NoTests = true
		log.Printf("[runner] WARN: no test files found in %s", abs)
		return result, nil
	}

	// Pass 2: scheduler trace on root package only.
	// -trace does not support ./... (multiple packages) — see ADR-008.
	//
	// We use -v to ensure the test binary writes trace data before exiting.
	// On fast machines (M-series Macs) the trace writer may not flush without it.
	log.Printf("[runner] pass 2: go test -trace=%s . in %s", traceFilePath, abs)
	p2args := []string{"test", "-count=1", "-timeout=60s", "-v",
		fmt.Sprintf("-trace=%s", traceFilePath)}
	if runFilter != "" {
		p2args = append(p2args, "-run", runFilter)
	}
	p2args = append(p2args, ".")
	_, _, _ = runGoTest(abs, p2args)
	// Ignore exit code from pass 2 — racy tests legitimately fail here.
	// We only care whether the trace file was written with real content.
	// Minimum viable trace is ~1KB — 64 bytes means the writer didn't flush.
	// A valid trace file must be at least 1KB.
	// On fast machines the trace writer sometimes flushes only the 69-byte header.
	// If that happens, retry once with a longer timeout to give the writer more time.
	const minViableTraceBytes = 1024

	traceSize := int64(0)
	if info, err := os.Stat(traceFilePath); err == nil {
		traceSize = info.Size()
	}

	if traceSize <= minViableTraceBytes {
		// Retry with a longer timeout — the trace writer needs more time on fast machines
		log.Printf("[runner] WARN: trace file too small (%d bytes), retrying with longer timeout...", traceSize)
		p2retry := []string{"test", "-count=1", "-timeout=120s", "-v",
			fmt.Sprintf("-trace=%s", traceFilePath)}
		if runFilter != "" {
			p2retry = append(p2retry, "-run", runFilter)
		}
		p2retry = append(p2retry, ".")
		_, _, _ = runGoTest(abs, p2retry)
		if info, err := os.Stat(traceFilePath); err == nil {
			traceSize = info.Size()
		}
	}

	if traceSize > minViableTraceBytes {
		result.TraceFile = traceFilePath
		log.Printf("[runner] trace file: %s (%d bytes)", traceFilePath, traceSize)
	} else {
		log.Printf("[runner] WARN: trace file empty after retry (%d bytes) — using synthetic timeline", traceSize)
	}

	return result, nil
}

// CollectTraceEvents converts the binary trace file to text using go tool trace.
//
// The -d flag changed in Go 1.23: integer mode (-d=1) became named mode (-d=parsed).
// We try -d=parsed first (works on 1.23+) and fall back to -d=1 (for older Go).
// See ADR-003 for why we use try/fallback instead of version string parsing.
func CollectTraceEvents(traceFile string) ([]byte, error) {
	if traceFile == "" {
		return nil, nil
	}

	// Try the newer flag first (Go 1.23+)
	if out, err := invokeTraceCmd(traceFile, "-d=parsed"); err == nil && len(out) > 0 {
		log.Printf("[runner] trace parsed with -d=parsed (%d bytes)", len(out))
		return out, nil
	}

	// Fall back to the older flag (Go ≤1.22)
	if out, err := invokeTraceCmd(traceFile, "-d=1"); err == nil && len(out) > 0 {
		log.Printf("[runner] trace parsed with -d=1 (%d bytes)", len(out))
		return out, nil
	}

	return nil, fmt.Errorf("runner: go tool trace failed with both -d=parsed and -d=1 — " +
		"check that the trace file is valid and your Go version supports text trace output")
}

// validateTarget checks that the target directory exists, contains Go files,
// and that the `go` binary is available on PATH.
func validateTarget(pkgPath string) error {
	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("runner: `go` not found on PATH — " +
			"install Go from https://go.dev/dl/ and ensure it is in your PATH")
	}

	info, err := os.Stat(pkgPath)
	if err != nil {
		return fmt.Errorf("runner: target path does not exist: %s", pkgPath)
	}
	if !info.IsDir() {
		return fmt.Errorf("runner: target must be a directory, not a file: %s", pkgPath)
	}

	goFiles, _ := filepath.Glob(filepath.Join(pkgPath, "*.go"))
	if len(goFiles) == 0 {
		return fmt.Errorf("runner: no .go files found in %s — "+
			"point racevis at a Go package directory", pkgPath)
	}

	return nil
}

// runGoTest executes a `go` command in the given directory and returns
// the combined stdout+stderr output. Exit code 2 (build failure) is
// returned as an error because it means racevis cannot proceed.
func runGoTest(dir string, args []string) ([]byte, int, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode := 0
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			if exitCode == 2 {
				// Build failure — surface the full error message
				combined := append(stdout.Bytes(), stderr.Bytes()...)
				return nil, 2, fmt.Errorf("runner: build failed — fix compilation errors before running racevis:\n%s",
					strings.TrimSpace(string(combined)))
			}
			// Exit code 1 = tests failed (expected when races are found)
		} else {
			return nil, -1, fmt.Errorf("runner: exec error: %w", err)
		}
	}

	// The race detector routes output through the test framework, which
	// combines it into stdout. Append stderr too for robustness across Go versions.
	combined := append(stdout.Bytes(), stderr.Bytes()...)
	return combined, exitCode, nil
}

// invokeTraceCmd runs `go tool trace <debugFlag> <traceFile>` and returns stdout.
func invokeTraceCmd(traceFile, debugFlag string) ([]byte, error) {
	cmd := exec.Command("go", "tool", "trace", debugFlag, traceFile)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil && out.Len() == 0 {
		return nil, fmt.Errorf("%s", strings.TrimSpace(errBuf.String()))
	}
	return out.Bytes(), nil
}
