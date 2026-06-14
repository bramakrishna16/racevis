// Package source reads Go source files and extracts code snippets around race sites.
// Snippets are embedded into the timeline JSON so the frontend can display
// the exact lines of code involved in each race condition.
package source

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Line is a single source line with its metadata.
type Line struct {
	Number  int    `json:"number"`
	Content string `json:"content"`
	IsRace  bool   `json:"isRace"` // true = this is the exact line the race was detected on
}

// Snippet is a window of source lines centered on a race site.
// Context lines above and below provide enough code to understand
// what the racing line is doing.
type Snippet struct {
	File     string `json:"file"`     // shortened path for display, e.g. "broker/broker.go"
	FullFile string `json:"fullFile"` // absolute path — used by VS Code to jump to line
	RaceLine int    `json:"raceLine"` // 1-indexed line number of the race
	Lines    []Line `json:"lines"`
}

// ReadSnippet reads ±context lines around raceLine in file.
//
// Returns (nil, nil) for stdlib files — these are not useful to show.
// Returns (nil, err) if the file exists but cannot be read.
// Returns (snippet, nil) on success.
//
// The caller should treat nil snippet as "source not available" —
// the race is still shown, just without source context.
func ReadSnippet(file string, raceLine, context int) (*Snippet, error) {
	if file == "" {
		return nil, nil
	}
	if isStdlib(file) {
		return nil, nil // stdlib — not useful to show, not an error
	}
	if raceLine < 1 {
		return nil, fmt.Errorf("source: invalid line number %d for %s", raceLine, file)
	}

	// Resolve to absolute path — race detector output may use relative paths
	absPath, err := resolveFile(file)
	if err != nil {
		// File not found — common when running racevis on a different machine
		// from where the test was run (CI artifacts). Not an error worth surfacing.
		log.Printf("[source] WARN: cannot resolve %s: %v", file, err)
		return nil, nil
	}

	f, err := os.Open(absPath)
	if err != nil {
		log.Printf("[source] WARN: cannot open %s: %v", absPath, err)
		return nil, nil
	}
	defer func() { _ = f.Close() }()

	var all []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		all = append(all, sc.Text())
	}
	if err := sc.Err(); err != nil {
		// Scanner error mid-read — partial data. Return error so caller
		// can decide whether to proceed with partial snippet or skip.
		return nil, fmt.Errorf("source: reading %s: %w", absPath, err)
	}

	if len(all) == 0 {
		return nil, fmt.Errorf("source: %s is empty", absPath)
	}

	raceIdx := raceLine - 1 // convert to 0-indexed
	if raceIdx >= len(all) {
		return nil, fmt.Errorf("source: line %d out of range (file has %d lines): %s",
			raceLine, len(all), absPath)
	}

	start := raceIdx - context
	if start < 0 {
		start = 0
	}
	end := raceIdx + context + 1
	if end > len(all) {
		end = len(all)
	}

	snippet := &Snippet{
		File:     displayPath(absPath),
		FullFile: absPath,
		RaceLine: raceLine,
	}
	for i := start; i < end; i++ {
		snippet.Lines = append(snippet.Lines, Line{
			Number:  i + 1,
			Content: all[i],
			IsRace:  i == raceIdx,
		})
	}

	return snippet, nil
}

// Key returns a stable deduplication key for a source location.
// Used by the snippet cache in main.go to avoid reading the same file twice.
func Key(file string, line int) string {
	return fmt.Sprintf("%s:%d", file, line)
}

// resolveFile tries the path as-is, then as an absolute path.
// Returns the resolved absolute path or an error if not found.
func resolveFile(file string) (string, error) {
	// Try as-is first (may already be absolute)
	if _, err := os.Stat(file); err == nil {
		abs, err := filepath.Abs(file)
		if err != nil {
			return file, nil // use as-is if Abs fails
		}
		return abs, nil
	}

	// Try as relative to cwd
	abs, err := filepath.Abs(file)
	if err != nil {
		return "", fmt.Errorf("cannot resolve path: %w", err)
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("file not found: %s (tried %s)", file, abs)
	}
	return abs, nil
}

// isStdlib reports whether a file path belongs to the Go standard library
// or runtime internals. These are excluded from snippet display because:
//  1. Developers need to see their own code, not Go internals
//  2. Stdlib files may not be present on stripped Go installations
//
// Uses prefix/component matching rather than Contains to avoid false positives
// (e.g. a user file named "runtime_test.go" should not be excluded).
func isStdlib(file string) bool {
	normalized := filepath.ToSlash(file)

	// Absolute stdlib path prefixes
	stdlibPrefixes := []string{
		"/usr/lib/go/",
		"/usr/local/go/",
		"/opt/homebrew/Cellar/go/",
	}
	for _, prefix := range stdlibPrefixes {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}

	// GOROOT env var — most reliable indicator.
	// goRoot() is safe for concurrent use (sync.Once internally).
	if goroot := goRoot(); goroot != "" {
		if strings.HasPrefix(normalized, filepath.ToSlash(goroot)) {
			return true
		}
	}

	// Path component checks — look for stdlib path segments
	// Use component matching to avoid false positives on user files
	parts := strings.Split(normalized, "/")
	for i, part := range parts {
		if part == "go" && i+1 < len(parts) {
			next := parts[i+1]
			if next == "src" || next == "pkg" || next == "libexec" {
				return true
			}
		}
	}

	return false
}

// displayPath returns a human-readable path for UI display.
// Shows the last two path components to identify the file without the full absolute path.
//
// Examples:
//
//	/home/user/myproject/internal/broker/broker.go → broker/broker.go
//	/home/user/myproject/main.go                   → myproject/main.go
func displayPath(absPath string) string {
	normalized := filepath.ToSlash(absPath)
	parts := strings.Split(normalized, "/")
	if len(parts) >= 2 {
		return strings.Join(parts[len(parts)-2:], "/")
	}
	return absPath
}

// goRoot returns the Go installation root directory.
// Safe for concurrent use — the value is computed exactly once via sync.Once.
// Caching avoids repeated os.Getenv calls on every snippet read.
var (
	gorootOnce   sync.Once
	cachedGoRoot string
)

func goRoot() string {
	gorootOnce.Do(func() {
		// GOROOT is set by the Go toolchain when running go test.
		// runtime.GOROOT() would be more reliable but creates an import
		// cycle since this package is used from main. GOROOT env var
		// is always set in the context where racevis runs (go test sets it).
		cachedGoRoot = os.Getenv("GOROOT")
	})
	return cachedGoRoot
}
