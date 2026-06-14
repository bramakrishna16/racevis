# racevis — Design Document

## Overview

racevis is a developer tool that visualizes Go race conditions and goroutine contention as an animated, interactive timeline. It bridges the gap between the Go race detector's static text output and a visual understanding of concurrent execution.

**Problem:** The Go race detector tells you *that* a race happened and *which lines* are involved. When you have multiple goroutines and shared state across several functions, mentally reconstructing the timeline from text is hard and slow.

**Solution:** Run the tests, capture both race events and scheduler timing, and render a frame-by-frame animation showing exactly which goroutines collided, on which memory address, and at what point in time.

---

## System Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                         racevis binary                          │
│                                                                 │
│  main.go                                                        │
│  ├── runner/     → executes go test, go tool trace              │
│  ├── parser/     → parses race output + scheduler trace         │
│  ├── source/     → reads source files, extracts code snippets   │
│  ├── correlator/ → joins both datasets into a Timeline struct   │
│  └── server/     → HTTP server: /api/timeline + static UI       │
│                                                                 │
│  ui/index.html   → single-file vanilla JS frontend             │
└─────────────────────────────────────────────────────────────────┘
```

### Data Flow

```
Target Go package
      │
      ├─ go test -race ./...          ← Pass 1 (runner)
      │         │
      │         ▼
      │   race detector stdout
      │         │
      │         ▼
      │   parser/race.go              ← State machine parser
      │         │
      │         ▼
      │   []RaceEvent
      │
      ├─ go test -trace=file .        ← Pass 2 (runner)
      │         │
      │         ▼
      │   go tool trace -d=parsed
      │         │
      │         ▼
      │   parser/trace.go             ← Multi-line block parser
      │         │
      │         ▼
      │   *TraceResult
      │
      └─ source/reader.go             ← Read source files
                │
                ▼
          *Timeline (correlator)      ← JSON-serialized to browser
                │
                ▼
          HTTP GET /api/timeline
                │
                ▼
          ui/index.html               ← SVG ECG animation
```

---

## Component Design

### runner — Test Execution

Runs two separate `go test` invocations. They cannot be combined because `-race` and `-trace` conflict on Go 1.23+ (see ADR-001).

**Key decisions:**
- Two passes, not one
- `-trace` targets `.` not `./...` (Go restriction — see ADR-008)
- Trace flag detection by try/fallback, not version parsing (see ADR-003)
- Exit code 1 = races found (expected). Exit code 2 = build failure (error).

**Inputs:** target directory path, trace output file path  
**Outputs:** raw race detector text, trace file path

### parser/race.go — Race Detector Parser

Parses the unstructured text output of the Go race detector using a state machine. Each `WARNING: DATA RACE` block maps to one `RaceEvent`.

**State machine:**
```
stateIdle
  → WARNING: DATA RACE
stateAccessA
  → "Write/Read at 0x... by goroutine N:"
stateAccessAStack
  → stack frames (function + file:line pairs)
  → "Previous read/write at..."
stateAccessBStack
  → stack frames
  → "Goroutine N (state) created at:"
stateCreationStack
  → creation stack frames
  → next "Goroutine N created at:" or "==="
stateCreationBStack
  → creation stack frames for goroutine B
```

Stack frames are two-line constructs: function name on line N, file:line on line N+1. A `pendingFunc` buffer holds the function name until its file:line arrives.

**Outputs:** `[]RaceEvent{ID, Address, AccessA, AccessB, CreationA, CreationB, RaceType}`

### parser/trace.go — Scheduler Trace Parser

Parses the text output of `go tool trace -d=parsed`. Handles multi-line stack blocks that follow `StateTransition` events.

**Handles two format eras:**
- Go ≤1.22: `Resource=Goroutine(N) Reason="..." GoID=N From->To`
- Go 1.23+: `GoID=N From->To Reason="..."`
- Fallback regex for future format changes

**Stack block parsing:**
```
StateTransition ... GoID=N NotExist->Runnable
TransitionStack=              ← goroutine's own entry point
    package.Function @ 0xaddr
        /path/file.go:42
                              ← blank line
Stack=                        ← spawner's call stack
    package.Caller @ 0xaddr
        /path/file.go:99
                              ← blank line (or next M= event)
```

**Outputs:** `*TraceResult{Events, Goroutines, MinTime, MaxTime}`  
Each `GoroutineInfo` includes a `CreationInfo` with `TestName` — used to group goroutines by test.

### source/reader.go — Source File Reader

Reads Go source files and extracts ±5 lines around the racing line. Results are embedded in the timeline JSON so the frontend can display the exact code without additional requests.

**Key behaviors:**
- Returns `(nil, nil)` for stdlib files — not useful to show
- Returns `(nil, error)` for file read failures — caller logs and continues
- `isStdlib()` uses path prefix matching, not `strings.Contains` (avoids false positives)
- Provides both `File` (display path) and `FullFile` (absolute path for VS Code jump-to-line)
- Cache in `main.go` prevents reading the same file twice (see ADR-009)

### correlator — Data Join

Takes `[]RaceEvent` and `*TraceResult` and produces a `*Timeline` for the frontend.

**Key operations:**

1. **`buildLanesFromTrace`** — converts per-goroutine state transition events into `[]GoroutineSegment{StartNs, EndNs, State}`. Only goroutines with a known test attribution are included.

2. **`buildCollisionZones`** — finds the time window where each pair of racing goroutines was simultaneously in Running state. This is approximate — the race detector provides no timestamps (see ADR-006). Uses `bestRunningWindow()` which returns the LAST running window, since races are detected after the fact.

3. **`buildGroups`** — clusters goroutines by test function using creation stack info. `HasRace` is set per-group (not globally) to prevent safe tests from inheriting race markers from other tests (see ADR-007).

4. **`annotateLanes`** — maps collision IDs to lanes using a pre-built map for O(n) performance (see ADR-012).

**Outputs:** `*Timeline{Groups, Lanes, Collisions, RaceEvents, MinTimeNs, MaxTimeNs, DurationNs, HasTraceData}`

### server — HTTP Server

Serves the timeline JSON at `/api/timeline` and the frontend HTML at `/`.

**Production features:**
- Request logging with method, path, status, duration
- Read/write/idle timeouts (prevents stuck VS Code webview connections)
- Pre-encoded JSON response (encoding errors don't produce partial 200 responses)
- CORS headers for VS Code webview origin
- Graceful shutdown via `Shutdown(ctx)`
- Port fallback: tries preferred port, then +9, then OS-assigned ephemeral

### ui/index.html — Frontend

Single-file vanilla JS application. No npm, no build step, no framework. Opens immediately after `go run .`.

**Rendering pipeline:**
1. `fetch('/api/timeline')` → JavaScript object
2. `renderTestList()` → left panel with dropdown filter
3. `renderTimeline(filterTest)` → SVG ECG lanes per goroutine
4. `requestAnimationFrame(tick)` → advances playhead, reveals trace

**ECG rendering (ADR-014, ADR-015):**
- Each goroutine is an SVG `<polyline>` in a `1000×32` viewBox
- Segments start invisible; the playhead reveals them as it advances
- Collision zones render as red bands, always visible
- One DOM write per goroutine per frame (vs 1,490 with div-based approach)

---

## Key Data Structures

```go
// One detected race — output of parser/race.go
type RaceEvent struct {
    ID        int
    Address   string       // hex memory address, e.g. "0x00c000112198"
    AccessA   RaceAccess   // triggering goroutine
    AccessB   RaceAccess   // previous conflicting goroutine
    CreationA []StackFrame // where goroutine A was spawned
    CreationB []StackFrame // where goroutine B was spawned
    RaceType  string       // "read/write", "write/write", etc.
}

// One goroutine's scheduler timeline — output of correlator
type GoroutineLane struct {
    ID           uint64
    Label        string             // "G11"
    Segments     []GoroutineSegment // state windows with timestamps
    CollisionIDs []int              // which races involve this goroutine
    BirthFunc    string             // e.g. "RunCounterRace.func1"
    HasRace      bool               // true = racing in its own test group
}

// Top-level output — serialized to JSON for the browser
type Timeline struct {
    Groups       []TestGroup     // goroutines grouped by test function
    Lanes        []GoroutineLane // flat list (all goroutines)
    Collisions   []CollisionZone // time windows of memory conflicts
    RaceEvents   []RaceEvent     // raw race detector output
    MinTimeNs    uint64
    MaxTimeNs    uint64
    DurationNs   uint64
    HasTraceData bool
}
```

---

## VS Code Extension Architecture

racevis is designed with VS Code deployment in mind. The extension would be a thin wrapper around the existing Go binary:

```
VS Code Extension (TypeScript)
├── extension.ts
│   ├── Registers "Analyze Races" command
│   ├── Spawns: racevis -target <workspace> -no-server
│   ├── Captures JSON from stdout
│   └── Opens WebviewPanel with ui/index.html
│
└── webview
    └── Receives JSON via panel.webview.postMessage()
        replaces: fetch('/api/timeline')
        with:     window.addEventListener('message', e => { tl = e.data; render(); })
```

**One-line change to ui/index.html for VS Code:**
```javascript
// Current (web server mode)
const r = await fetch('/api/timeline');
tl = await r.json();

// VS Code webview mode
window.addEventListener('message', e => { tl = e.data; render(); });
```

**Additional VS Code features enabled by `source/reader.go`:**
- `Snippet.FullFile` (absolute path) enables jump-to-line:
  ```typescript
  vscode.window.showTextDocument(
    Uri.file(snippet.fullFile),
    { selection: new Range(snippet.raceLine - 1, 0, snippet.raceLine - 1, 999) }
  );
  ```

---

## Error Handling Philosophy

| Situation | Behavior |
|-----------|----------|
| Go not on PATH | Error with install instructions |
| Target dir doesn't exist | Error with path |
| No .go files in target | Error with guidance |
| No test files | Warning, continue (shows empty timeline) |
| Build failure | Error with compiler output |
| Races found (exit 1) | Expected — parse and continue |
| Trace collection fails | Warning, use synthetic timeline |
| Source file not found | Warning, show race without source |
| Stdlib source requested | Silent nil — not an error |
| Partial JSON encoding | 500 error with proper HTTP status |

---

## Performance Characteristics

| Operation | Complexity | Notes |
|-----------|-----------|-------|
| Race output parsing | O(lines) | State machine, one pass |
| Trace parsing | O(events) | One pass with stack block buffering |
| `collectRelevantGoroutines` | O(goroutines) | Map lookup |
| `buildLanesFromTrace` | O(goroutines × events) | Per-goroutine segment building |
| `buildCollisionZones` | O(races × goroutine_events) | `bestRunningWindow` scan |
| `annotateLanes` | O(collisions + lanes) | Pre-built map, not nested loop |
| `buildGroups` | O(goroutines + collisions) | Two passes |
| SVG frame update | O(goroutines) | One string write per goroutine |

For production-scale trace files (50,000+ goroutines), `buildCollisionZones` is the bottleneck. An indexed interval tree on goroutine running windows would reduce it to O(races × log(goroutine_events)).

---

## Known Limitations

1. **Collision zone placement is approximate.** The race detector provides no timestamps. Zones are placed at the overlap of the last running windows of the two racing goroutines (see ADR-006).

2. **`-trace` covers root package only.** Go does not allow `-trace` with `./...`. Goroutines from sub-packages appear in the timeline only if invoked from root package tests (see ADR-008).

3. **Two test runs, non-deterministic.** Race conditions are timing-dependent. Pass 1 may catch different races than what would appear in a single combined run.

4. **Creation stack parsing may misattribute goroutines.** The stack parser uses `strings.HasPrefix(fn, "Test")` to find the owning test. Anonymous functions and non-standard naming can cause misattribution.

5. **No support for `-count > 1`.** Each pass uses `-count=1` to ensure a fresh run. Race detection improves with multiple runs — a future `-count N` flag would help surface intermittent races.
