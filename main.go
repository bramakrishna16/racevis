// racevis — Go race condition visualizer.
//
// Usage:
//
//	racevis                          # analyze bundled demo, open http://localhost:7777
//	racevis -target ./mypackage      # analyze your own Go package
//	racevis -run TestFoo             # analyze one specific test (faster feedback)
//	racevis -count 3                 # run race detector 3 times (catches intermittent races)
//	racevis -watch                   # re-analyze on every .go file change
//	racevis -report report.html      # export self-contained HTML report
//	racevis -json out.json           # export timeline JSON
//	racevis -no-server               # print timeline JSON to stdout
//	racevis -dwarf                   # resolve memory addresses to variable names (experimental)
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bramakrishna16/racevis/correlator"
	"github.com/bramakrishna16/racevis/parser"
	"github.com/bramakrishna16/racevis/runner"
	"github.com/bramakrishna16/racevis/server"
	"github.com/bramakrishna16/racevis/source"
)

// uiHTML is the embedded frontend. Makes the compiled binary self-contained —
// works from any directory without needing ui/index.html on disk.
//
//go:embed ui/index.html
var uiHTML []byte

func main() {
	var (
		targetPath = flag.String("target", "", "Go package directory to analyze (default: bundled demo)")
		runFilter  = flag.String("run", "", "Run only tests matching this regex — passed as go test -run (e.g. TestBrokerRace)")
		outputJSON = flag.String("json", "", "Write timeline JSON to this file instead of starting the web server")
		reportFile = flag.String("report", "", "Export a self-contained HTML report to this file")
		port       = flag.Int("port", 7777, "Port for the web UI (tries next available if taken)")
		noServer   = flag.Bool("no-server", false, "Print timeline JSON to stdout, do not start server")
		watchMode  = flag.Bool("watch", false, "Re-analyze whenever a .go file in the target package changes")
		runCount   = flag.Int("count", 1, "Number of times to run go test -race (higher = catches more intermittent races)")
		useDWARF   = flag.Bool("dwarf", false, "Resolve memory addresses to variable names using DWARF debug info (experimental)")
	)
	flag.Parse()

	log.SetPrefix("[racevis] ")
	log.SetFlags(0)

	if *runCount < 1 {
		log.Fatalf("-count must be at least 1")
	}

	// Resolve target directory
	target := resolveTarget(*targetPath)
	abs, err := filepath.Abs(target)
	if err != nil {
		log.Fatalf("resolving target path: %v", err)
	}
	log.Printf("target: %s", abs)

	if *watchMode {
		runWatchMode(abs, *runFilter, *runCount, *useDWARF, *port)
		return
	}

	// Single analysis run
	timeline, err := analyze(abs, *runFilter, *runCount, *useDWARF)
	if err != nil {
		log.Fatalf("%v", err)
	}

	// Output modes
	switch {
	case *reportFile != "":
		if err := exportHTMLReport(timeline, *reportFile); err != nil {
			log.Fatalf("exporting HTML report: %v", err)
		}
		log.Printf("report written to %s", *reportFile)

	case *noServer || *outputJSON != "":
		data, err := json.MarshalIndent(timeline, "", "  ")
		if err != nil {
			log.Fatalf("marshaling timeline: %v", err)
		}
		if *outputJSON != "" {
			if err := os.WriteFile(*outputJSON, data, 0644); err != nil {
				log.Fatalf("writing JSON: %v", err)
			}
			log.Printf("timeline written to %s", *outputJSON)
		} else {
			fmt.Println(string(data))
		}

	default:
		startServer(timeline, *port)
	}
}

// analyze runs both passes and returns a complete Timeline.
func analyze(abs, runFilter string, count int, useDWARF bool) (*correlator.Timeline, error) {
	// Create temp file for runtime trace
	tmp, err := os.CreateTemp("", "racevis-trace-*.out")
	if err != nil {
		return nil, fmt.Errorf("creating temp trace file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("closing temp trace file: %w", err)
	}
	traceFilePath := tmp.Name()
	defer func() { _ = os.Remove(traceFilePath) }()

	// Pass 1: race detection
	log.Printf("pass 1/2: go test -race (detecting races)...")
	result, err := runner.RunTarget(abs, traceFilePath, count, runFilter)
	if err != nil {
		return nil, fmt.Errorf("test run failed: %v", err)
	}
	log.Printf("pass 1 complete (exit code %d, %d bytes of output)", result.ExitCode, len(result.RaceOutput))

	if result.NoTests {
		log.Println("  → no test files found — add tests that exercise concurrent code")
	}

	// Parse race detector output
	races, err := parser.ParseRaceOutput(bytes.NewReader(result.RaceOutput))
	if err != nil {
		return nil, fmt.Errorf("parsing race output: %v", err)
	}
	log.Printf("found %d race events", len(races))

	// Pass 2: scheduler trace
	var traceResult *parser.TraceResult
	if result.TraceFile != "" {
		log.Printf("pass 2/2: parsing scheduler trace (%s)...", result.TraceFile)
		traceData, err := runner.CollectTraceEvents(result.TraceFile)
		if err != nil {
			log.Printf("  → trace parse failed: %v", err)
		} else if len(traceData) > 0 {
			traceResult, err = parser.ParseTraceOutput(bytes.NewReader(traceData))
			if err != nil {
				log.Printf("  → trace event parse failed: %v", err)
			} else {
				log.Printf("  → %d scheduler events across %d goroutines",
					len(traceResult.Events), len(traceResult.Goroutines))
			}
		}
	} else {
		log.Printf("pass 2/2: no trace file — using synthetic timeline")
	}

	// Enrich with source snippets
	enrichWithSource(races)

	// DWARF variable name resolution (optional — behind -dwarf flag)
	if useDWARF {
		log.Printf("resolving memory addresses to variable names (DWARF)...")
		if err := enrichWithDWARF(races, abs); err != nil {
			log.Printf("  → DWARF resolution failed: %v (addresses will show as hex)", err)
		}
	}

	// Correlate
	timeline := correlator.Correlate(races, traceResult)
	log.Printf("timeline: %d goroutine lanes, %d collision zones, hasTraceData=%v",
		len(timeline.Lanes), len(timeline.Collisions), timeline.HasTraceData)

	return timeline, nil
}

// runWatchMode polls the target directory for .go file changes and re-analyzes
// on every change. The server pushes the new timeline to the browser via
// Server-Sent Events so the page refreshes automatically.
func runWatchMode(abs, runFilter string, count int, useDWARF bool, port int) {
	log.Printf("watch mode — monitoring %s for .go file changes", abs)
	log.Printf("press Ctrl+C to stop")

	// Initial analysis
	timeline, err := analyze(abs, runFilter, count, useDWARF)
	if err != nil {
		log.Printf("initial analysis failed: %v", err)
		timeline = &correlator.Timeline{} // serve empty timeline so UI still loads
	}

	srv := server.New(timeline, port)
	srv.SetEmbeddedUI(uiHTML)

	// Handle Ctrl+C
	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit
		log.Println("shutting down...")
		os.Exit(0)
	}()

	// File watcher — polls every 500ms
	go func() {
		snapshots := goFileSnapshots(abs)
		for {
			time.Sleep(500 * time.Millisecond)
			current := goFileSnapshots(abs)
			if !snapshotsEqual(snapshots, current) {
				snapshots = current
				log.Printf("[watch] change detected — re-analyzing...")
				newTimeline, err := analyze(abs, runFilter, count, useDWARF)
				if err != nil {
					log.Printf("[watch] analysis failed: %v", err)
					continue
				}
				srv.UpdateTimeline(newTimeline)
				log.Printf("[watch] updated — browser will refresh automatically")
			}
		}
	}()

	if err := srv.Start(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// goFileSnapshots returns a map of .go file path → modification time
// for all .go files in the given directory (non-recursive for speed).
func goFileSnapshots(dir string) map[string]time.Time {
	snaps := make(map[string]time.Time)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return snaps
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			if info, err := e.Info(); err == nil {
				snaps[e.Name()] = info.ModTime()
			}
		}
	}
	return snaps
}

// snapshotsEqual returns true if two file snapshot maps are identical.
func snapshotsEqual(a, b map[string]time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || !v.Equal(bv) {
			return false
		}
	}
	return true
}

// startServer starts the HTTP server and blocks until exit.
func startServer(timeline *correlator.Timeline, port int) {
	srv := server.New(timeline, port)
	srv.SetEmbeddedUI(uiHTML)

	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit
		log.Println("shutting down...")
		os.Exit(0)
	}()

	if err := srv.Start(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// exportHTMLReport writes a self-contained HTML file with the timeline JSON
// baked in. The file works completely offline — no server needed.
//
// Strategy: inject the timeline JSON as a <script> tag BEFORE the main
// <script> block so that window.__RACEVIS_INLINE_DATA__ is defined before
// init() runs and the inline check at the top of init() picks it up.
func exportHTMLReport(timeline *correlator.Timeline, outPath string) error {
	data, err := json.Marshal(timeline)
	if err != nil {
		return fmt.Errorf("marshaling timeline: %w", err)
	}

	html := string(uiHTML)

	// Inject the data script BEFORE the main <script> tag so the variable
	// is defined when init() executes. Injecting after </body> is too late.
	inlineScript := fmt.Sprintf("<script>\nwindow.__RACEVIS_INLINE_DATA__ = %s;\n</script>\n", string(data))
	html = strings.Replace(html, "<script>", inlineScript+"<script>", 1)

	return os.WriteFile(outPath, []byte(html), 0644)
}

// resolveTarget returns the absolute path of the target package.
func resolveTarget(target string) string {
	if target != "" {
		return target
	}
	self, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(self), "..", "testdata", "racy")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "testdata/racy"
}

// enrichWithSource reads source files and embeds code snippets into each race.
func enrichWithSource(races []parser.RaceEvent) {
	cache := make(map[string]*source.Snippet)

	getSnippet := func(file string, line int) *source.Snippet {
		key := source.Key(file, line)
		if s, ok := cache[key]; ok {
			return s
		}
		s, err := source.ReadSnippet(file, line, 5)
		if err != nil {
			log.Printf("[source] %s:%d: %v", file, line, err)
		}
		cache[key] = s
		return s
	}

	for i := range races {
		for _, access := range []*parser.RaceAccess{&races[i].AccessA, &races[i].AccessB} {
			if len(access.Stack) == 0 {
				continue
			}
			frame := access.Stack[0]
			if s := getSnippet(frame.File, frame.Line); s != nil {
				access.Snippet = &parser.SourceSnippet{
					File:     s.File,
					FullFile: s.FullFile,
					RaceLine: s.RaceLine,
				}
				for _, l := range s.Lines {
					access.Snippet.Lines = append(access.Snippet.Lines, parser.SnippetLine{
						Number:  l.Number,
						Content: l.Content,
						IsRace:  l.IsRace,
					})
				}
			}
		}
	}
}

// enrichWithDWARF attempts to resolve memory addresses to variable names.
// This is experimental — it works reliably for simple scalars but gives
// only the container name for maps, slices, and interfaces.
// Gated behind -dwarf flag so users opt in deliberately.
func enrichWithDWARF(races []parser.RaceEvent, pkgDir string) error {
	resolver, err := newDWARFResolver(pkgDir)
	if err != nil {
		return err
	}
	defer resolver.Close()

	for i := range races {
		if name := resolver.Resolve(races[i].Address); name != "" {
			races[i].VarName = name
			log.Printf("[dwarf] %s → %s", races[i].Address, name)
		}
	}
	return nil
}
