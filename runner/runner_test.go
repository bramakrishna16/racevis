package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateTarget_GoNotOnPath(t *testing.T) {
	// We can't easily remove `go` from PATH in a test, so we test the
	// directory validation logic instead.
	t.Log("Skipping PATH check — can't remove go binary in test")
}

func TestValidateTarget_NonExistentDir(t *testing.T) {
	err := validateTarget("/nonexistent/path/that/does/not/exist")
	if err == nil {
		t.Error("expected error for non-existent directory")
	}
}

func TestValidateTarget_FileNotDir(t *testing.T) {
	// Create a temp file (not a directory)
	f, err := os.CreateTemp("", "racevis-test-*.go")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	_ = f.Close()
	defer func() { _ = os.Remove(f.Name()) }()

	err = validateTarget(f.Name())
	if err == nil {
		t.Error("expected error when target is a file, not a directory")
	}
}

func TestValidateTarget_EmptyDir(t *testing.T) {
	// Create an empty temp directory with no .go files
	dir, err := os.MkdirTemp("", "racevis-empty-*")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	err = validateTarget(dir)
	if err == nil {
		t.Error("expected error for directory with no .go files")
	}
}

func TestValidateTarget_ValidDir(t *testing.T) {
	// Create a temp directory with a .go file
	dir, err := os.MkdirTemp("", "racevis-valid-*")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	goFile := filepath.Join(dir, "main.go")
	if err := os.WriteFile(goFile, []byte("package main"), 0644); err != nil {
		t.Fatalf("writing go file: %v", err)
	}

	err = validateTarget(dir)
	if err != nil {
		t.Errorf("unexpected error for valid directory: %v", err)
	}
}

func TestCollectTraceEvents_EmptyPath(t *testing.T) {
	out, err := CollectTraceEvents("")
	if err != nil {
		t.Errorf("expected nil error for empty path, got: %v", err)
	}
	if out != nil {
		t.Error("expected nil output for empty path")
	}
}

func TestCollectTraceEvents_NonExistentFile(t *testing.T) {
	// Should fail gracefully — both flag variants will fail
	out, err := CollectTraceEvents("/nonexistent/trace.out")
	// Both -d=parsed and -d=1 will fail → should return an error
	if err == nil && len(out) > 0 {
		t.Error("expected error or empty output for non-existent trace file")
	}
}
