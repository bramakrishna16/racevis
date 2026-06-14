package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemp creates a temp file with the given content and returns its path.
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", "racevis-test-*.go")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	_ = f.Close()
	t.Cleanup(func() { _ = os.Remove(f.Name()) })
	return f.Name()
}

const sampleGo = `package main

import "fmt"

func main() {
	counter := 0
	counter++ // line 7 — the race line
	fmt.Println(counter)
}
`

func TestReadSnippet_HappyPath(t *testing.T) {
	path := writeTemp(t, sampleGo)

	snippet, err := ReadSnippet(path, 7, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snippet == nil {
		t.Fatal("expected snippet, got nil")
	}
	if snippet.RaceLine != 7 {
		t.Errorf("RaceLine: want 7, got %d", snippet.RaceLine)
	}
}

func TestReadSnippet_RaceLineMarked(t *testing.T) {
	path := writeTemp(t, sampleGo)
	snippet, _ := ReadSnippet(path, 7, 2)

	var raceLine *Line
	for i := range snippet.Lines {
		if snippet.Lines[i].IsRace {
			raceLine = &snippet.Lines[i]
			break
		}
	}
	if raceLine == nil {
		t.Fatal("no line marked as IsRace")
	}
	if raceLine.Number != 7 {
		t.Errorf("race line number: want 7, got %d", raceLine.Number)
	}
	if !strings.Contains(raceLine.Content, "counter++") {
		t.Errorf("race line content: expected counter++, got %q", raceLine.Content)
	}
}

func TestReadSnippet_ContextLines(t *testing.T) {
	path := writeTemp(t, sampleGo)
	snippet, _ := ReadSnippet(path, 7, 2) // ±2 lines

	// Should have lines 5..9 (context 2 above and below line 7)
	if len(snippet.Lines) < 3 {
		t.Errorf("expected at least 3 lines with context=2, got %d", len(snippet.Lines))
	}
}

func TestReadSnippet_EmptyFile(t *testing.T) {
	path := writeTemp(t, "")
	snippet, err := ReadSnippet(path, 1, 2)
	if err == nil {
		t.Error("expected error for empty file, got nil")
	}
	if snippet != nil {
		t.Error("expected nil snippet for empty file")
	}
}

func TestReadSnippet_LineOutOfRange(t *testing.T) {
	path := writeTemp(t, sampleGo)
	// File has ~9 lines, request line 999
	snippet, err := ReadSnippet(path, 999, 2)
	if err == nil {
		t.Error("expected error for out-of-range line")
	}
	if snippet != nil {
		t.Error("expected nil snippet for out-of-range line")
	}
}

func TestReadSnippet_InvalidLineNumber(t *testing.T) {
	path := writeTemp(t, sampleGo)
	snippet, err := ReadSnippet(path, 0, 2) // line 0 is invalid
	if err == nil {
		t.Error("expected error for line 0")
	}
	if snippet != nil {
		t.Error("expected nil snippet for line 0")
	}
}

func TestReadSnippet_EmptyPath(t *testing.T) {
	snippet, err := ReadSnippet("", 1, 2)
	if err != nil {
		t.Errorf("expected nil error for empty path, got: %v", err)
	}
	if snippet != nil {
		t.Error("expected nil snippet for empty path")
	}
}

func TestReadSnippet_MissingFile(t *testing.T) {
	snippet, err := ReadSnippet("/nonexistent/path/file.go", 1, 2)
	// Missing file should return nil,nil (not an error — file may be on CI machine)
	if err != nil {
		t.Errorf("expected nil error for missing file, got: %v", err)
	}
	if snippet != nil {
		t.Error("expected nil snippet for missing file")
	}
}

func TestReadSnippet_FullFilePath(t *testing.T) {
	path := writeTemp(t, sampleGo)
	snippet, _ := ReadSnippet(path, 7, 2)
	if snippet == nil {
		t.Fatal("expected snippet")
	}
	if !filepath.IsAbs(snippet.FullFile) {
		t.Errorf("FullFile should be absolute, got: %s", snippet.FullFile)
	}
}

func TestReadSnippet_DisplayPath(t *testing.T) {
	path := writeTemp(t, sampleGo)
	snippet, _ := ReadSnippet(path, 7, 2)
	if snippet == nil {
		t.Fatal("expected snippet")
	}
	// Display path should be short — at most 2 path components
	parts := strings.Split(filepath.ToSlash(snippet.File), "/")
	if len(parts) > 2 {
		t.Errorf("display path too long: %s (want at most 2 components)", snippet.File)
	}
}

func TestIsStdlib_KnownStdlib(t *testing.T) {
	cases := []struct {
		path   string
		stdlib bool
	}{
		{"/usr/local/go/src/runtime/map.go", true},
		{"/usr/lib/go/src/sync/mutex.go", true},
		{"/opt/homebrew/Cellar/go/1.22/libexec/src/fmt/print.go", true},
		{"/home/user/myproject/main.go", false},
		{"/home/user/myproject/runtime_helpers.go", false}, // should NOT be flagged
		{"", false},
	}

	for _, c := range cases {
		got := isStdlib(c.path)
		if got != c.stdlib {
			t.Errorf("isStdlib(%q): want %v, got %v", c.path, c.stdlib, got)
		}
	}
}

func TestKey_Format(t *testing.T) {
	k := Key("/home/user/main.go", 42)
	if k != "/home/user/main.go:42" {
		t.Errorf("Key format wrong: %s", k)
	}
}

func TestDisplayPath_ShortensLongPaths(t *testing.T) {
	long := "/home/user/myproject/internal/broker/broker.go"
	got := displayPath(long)
	if got != "broker/broker.go" {
		t.Errorf("displayPath(%q) = %q, want broker/broker.go", long, got)
	}
}

func TestDisplayPath_ShortPath(t *testing.T) {
	short := "main.go"
	got := displayPath(short)
	// Short paths should not panic or be mangled
	if got == "" {
		t.Errorf("displayPath(%q) returned empty string", short)
	}
}
