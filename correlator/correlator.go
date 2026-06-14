// Package correlator stitches race detector events with scheduler trace events
// into a unified Timeline struct that the frontend renders.
//
// Data flow:
//
//	[]parser.RaceEvent   (from race detector stderr)
//	*parser.TraceResult  (from runtime/trace via go tool trace)
//	     ↓ Correlate()
//	*Timeline            (JSON-serialized to browser)
//
// The key join: both datasets share goroutine IDs. The correlator uses those
// IDs to find the scheduler windows where racing goroutines overlapped in time.
package correlator

import (
	"fmt"
	"github.com/bramakrishna16/racevis/parser"
	"log"
	"sort"
	"strings"
)

// ---- Core data types ----

// GoroutineSegment is one contiguous period of a goroutine in a single scheduler state.
// The frontend renders each segment as a colored section of the ECG line.
type GoroutineSegment struct {
	StartNs uint64         `json:"startNs"`
	EndNs   uint64         `json:"endNs"`
	State   parser.GoState `json:"state"`
	Reason  string         `json:"reason,omitempty"` // why it blocked: "chan receive", "sync.Mutex", etc.
	ProcID  int            `json:"procId"`           // which P (logical processor) it ran on
}

// CollisionZone is a time window where two goroutines concurrently accessed
// the same memory address. Rendered as a red band on the ECG timeline.
type CollisionZone struct {
	StartNs     uint64 `json:"startNs"`
	EndNs       uint64 `json:"endNs"`
	Address     string `json:"address"`     // hex memory address, e.g. "0x00c000112198"
	RaceType    string `json:"raceType"`    // "read/write", "write/write", etc.
	GoroutineA  uint64 `json:"goroutineA"`  // triggering goroutine
	GoroutineB  uint64 `json:"goroutineB"`  // previous conflicting goroutine
	RaceEventID int    `json:"raceEventId"` // links back to the RaceEvent
}

// GoroutineLane is one goroutine's full timeline — segments + collision membership.
// Rendered as a single horizontal ECG line in the UI.
type GoroutineLane struct {
	ID           uint64             `json:"id"`
	Label        string             `json:"label"` // "G11"
	Segments     []GoroutineSegment `json:"segments"`
	CollisionIDs []int              `json:"collisionIds"`        // race event IDs this goroutine appears in
	BirthFunc    string             `json:"birthFunc,omitempty"` // e.g. "RunCounterRace.func1"
	BirthFile    string             `json:"birthFile,omitempty"` // source file where `go func()` was called
	BirthLine    int                `json:"birthLine,omitempty"` // line number of the `go` statement
	HasRace      bool               `json:"hasRace"`             // true = involved in a race in its own test group
}

// TestGroup clusters all goroutines spawned by a single test function.
// The left panel in the UI shows one entry per TestGroup.
type TestGroup struct {
	TestName     string          `json:"testName"`
	Lanes        []GoroutineLane `json:"lanes"`
	Collisions   []CollisionZone `json:"collisions"`
	RaceEventIDs []int           `json:"raceEventIds"` // which races belong to this group
}

// Timeline is the top-level output consumed by the frontend.
// Groups is the primary view (by test); Lanes/Collisions are the flat view.
type Timeline struct {
	Groups       []TestGroup        `json:"groups"`
	Lanes        []GoroutineLane    `json:"lanes"`      // all goroutines, ungrouped
	Collisions   []CollisionZone    `json:"collisions"` // all collision zones
	RaceEvents   []parser.RaceEvent `json:"raceEvents"` // raw race detector output
	MinTimeNs    uint64             `json:"minTimeNs"`
	MaxTimeNs    uint64             `json:"maxTimeNs"`
	DurationNs   uint64             `json:"durationNs"`
	HasTraceData bool               `json:"hasTraceData"` // false → synthetic timeline (no real timestamps)
}

// ---- Entry point ----

// Correlate joins race events and scheduler trace data into a Timeline.
// traceResult may be nil — in that case a synthetic timeline is produced
// (race topology only, no real timing axis).
func Correlate(races []parser.RaceEvent, tr *parser.TraceResult) *Timeline {
	tl := &Timeline{RaceEvents: races}

	if len(races) == 0 {
		log.Println("[correlator] no race events — timeline will be empty")
	}

	if tr != nil && len(tr.Events) > 0 {
		tl.HasTraceData = true
		tl.MinTimeNs = tr.MinTime
		tl.MaxTimeNs = tr.MaxTime
		tl.DurationNs = tr.MaxTime - tr.MinTime
		buildLanesFromTrace(tl, tr, races)
		log.Printf("[correlator] built %d lanes from trace data (duration: %dns)",
			len(tl.Lanes), tl.DurationNs)
	} else {
		log.Println("[correlator] no trace data — using synthetic timeline")
		buildSyntheticLanes(tl, races)
	}

	buildCollisionZones(tl, races, tr)
	annotateLanes(tl)
	buildGroups(tl, tr)

	log.Printf("[correlator] result: %d groups, %d lanes, %d collision zones",
		len(tl.Groups), len(tl.Lanes), len(tl.Collisions))

	return tl
}

// ---- Lane construction ----

// buildLanesFromTrace constructs goroutine lanes from real scheduler events.
// Only goroutines relevant to the race events (and their test siblings) are included
// to keep the visualization focused and avoid overwhelming the UI with runtime internals.
func buildLanesFromTrace(tl *Timeline, tr *parser.TraceResult, races []parser.RaceEvent) {
	relevant := collectRelevantGoroutines(races, tr)
	log.Printf("[correlator] %d relevant goroutines out of %d total in trace",
		len(relevant), len(tr.Goroutines))

	for gid := range relevant {
		info, ok := tr.Goroutines[gid]
		if !ok {
			// Goroutine appeared in race output but not in scheduler trace.
			// This can happen when the race fires very late in execution.
			log.Printf("[correlator] G%d: in race events but not in trace — adding empty lane", gid)
			tl.Lanes = append(tl.Lanes, GoroutineLane{ID: gid, Label: gLabel(gid)})
			continue
		}

		lane := GoroutineLane{ID: gid, Label: gLabel(gid)}

		// Attach birth site from creation stack
		if info.Creation != nil {
			lane.BirthFunc = info.Creation.BirthFunc
			lane.BirthFile = info.Creation.BirthFile
			lane.BirthLine = info.Creation.BirthLine
		}

		// Convert state transition events into contiguous segments.
		// Each event marks when the goroutine LEFT the previous state.
		// So segment[i] = [event[i].ts, event[i+1].ts] in state event[i].ToState.
		for i, ev := range info.Events {
			var endNs uint64
			if i+1 < len(info.Events) {
				endNs = info.Events[i+1].TimestampNs
			} else {
				endNs = tr.MaxTime
			}

			// First event: add a segment for the initial state before the first transition
			if i == 0 && ev.FromState != parser.GoStateNotExist {
				lane.Segments = append(lane.Segments, GoroutineSegment{
					StartNs: tr.MinTime,
					EndNs:   ev.TimestampNs,
					State:   ev.FromState,
					ProcID:  ev.ProcID,
				})
			}

			lane.Segments = append(lane.Segments, GoroutineSegment{
				StartNs: ev.TimestampNs,
				EndNs:   endNs,
				State:   ev.ToState,
				Reason:  ev.Reason,
				ProcID:  ev.ProcID,
			})
		}

		tl.Lanes = append(tl.Lanes, lane)
	}

	// Stable sort by goroutine ID for deterministic rendering order
	sort.Slice(tl.Lanes, func(i, j int) bool {
		return tl.Lanes[i].ID < tl.Lanes[j].ID
	})
}

// buildSyntheticLanes creates placeholder lanes when no trace data is available.
// Each lane gets a single "running" segment spanning a synthetic 1-second timeline.
// This lets the UI show race topology (which goroutines, which addresses) without
// real timing data.
func buildSyntheticLanes(tl *Timeline, races []parser.RaceEvent) {
	seen := make(map[uint64]bool)
	const syntheticDuration = uint64(1_000_000_000) // 1 second

	tl.MinTimeNs = 0
	tl.MaxTimeNs = syntheticDuration
	tl.DurationNs = syntheticDuration

	for _, race := range races {
		for _, gid := range []uint64{race.AccessA.GoroutineID, race.AccessB.GoroutineID} {
			if seen[gid] || gid == 0 {
				continue
			}
			seen[gid] = true
			tl.Lanes = append(tl.Lanes, GoroutineLane{
				ID:    gid,
				Label: gLabel(gid),
				Segments: []GoroutineSegment{{
					StartNs: 0,
					EndNs:   syntheticDuration,
					State:   parser.GoStateRunning,
				}},
			})
		}
	}

	sort.Slice(tl.Lanes, func(i, j int) bool {
		return tl.Lanes[i].ID < tl.Lanes[j].ID
	})
}

// ---- Collision zone computation ----

// buildCollisionZones finds the time window where each pair of racing goroutines
// overlapped in Running state. That overlap IS the collision — the moment they
// both had CPU time and touched the same address.
//
// When both goroutines have overlapping running windows → use the intersection.
// When they don't overlap (scheduler separated them) → use the start of goroutine B's
// window as the best approximation of when the race was detected.
func buildCollisionZones(tl *Timeline, races []parser.RaceEvent, tr *parser.TraceResult) {
	for _, race := range races {
		zone := CollisionZone{
			Address:     race.Address,
			RaceType:    race.RaceType,
			GoroutineA:  race.AccessA.GoroutineID,
			GoroutineB:  race.AccessB.GoroutineID,
			RaceEventID: race.ID,
		}

		if tr != nil {
			// Find the running windows of both goroutines around the collision time.
			// We use the window closest to when the race detector fired, not just
			// the first running window (a goroutine may run many times).
			sA, eA := bestRunningWindow(race.AccessA.GoroutineID, tr)
			sB, eB := bestRunningWindow(race.AccessB.GoroutineID, tr)

			overlapStart := max64(sA, sB)
			overlapEnd := min64(eA, eB)

			if overlapStart < overlapEnd {
				// Goroutines were truly concurrent — show the intersection
				zone.StartNs = overlapStart
				zone.EndNs = overlapEnd
			} else {
				// Scheduler separated them; approximate with a 5% window at goroutine B's start
				zone.StartNs = sB
				zone.EndNs = sB + tl.DurationNs/20
				log.Printf("[correlator] race #%d: G%d and G%d have no overlapping running window — using approximation",
					race.ID, race.AccessA.GoroutineID, race.AccessB.GoroutineID)
			}
		} else {
			// No trace data — place collision in the middle of the synthetic timeline
			mid := tl.DurationNs / 2
			spread := tl.DurationNs / 10
			zone.StartNs = mid - spread
			zone.EndNs = mid + spread
		}

		tl.Collisions = append(tl.Collisions, zone)
	}
}

// ---- Lane annotation ----

// annotateLanes adds CollisionIDs to lanes — which race events involve each goroutine.
// HasRace is intentionally NOT set here; it is set per-group in buildGroups to avoid
// cross-test contamination (a goroutine in a safe test should not be marked HasRace
// because of a collision in a different test).
func annotateLanes(tl *Timeline) {
	// Build a lookup: goroutine ID → collision IDs
	// O(C) space where C = number of collisions
	gidToCollisions := make(map[uint64][]int)
	for _, c := range tl.Collisions {
		gidToCollisions[c.GoroutineA] = append(gidToCollisions[c.GoroutineA], c.RaceEventID)
		gidToCollisions[c.GoroutineB] = append(gidToCollisions[c.GoroutineB], c.RaceEventID)
	}

	for i := range tl.Lanes {
		tl.Lanes[i].CollisionIDs = gidToCollisions[tl.Lanes[i].ID]
	}
}

// ---- Group construction ----

// buildGroups clusters goroutines by the test function that spawned them.
// This produces the left-panel test list and per-test timeline sections.
//
// Source of test name (in priority order):
//  1. Creation stack from runtime/trace (most reliable — direct parent chain)
//  2. Creation stack from race detector output (fallback — only for racy goroutines)
//  3. "other" bucket (runtime goroutines, GC, timer, etc.)
func buildGroups(tl *Timeline, tr *parser.TraceResult) {
	// Step 1: build goroutine → test name mapping
	gidToTest := make(map[uint64]string)

	if tr != nil {
		for gid, info := range tr.Goroutines {
			if info.Creation != nil && info.Creation.TestName != "" {
				gidToTest[gid] = info.Creation.TestName
			}
		}
	}

	// Fallback: infer from race event creation stacks for goroutines not in trace
	for _, race := range tl.RaceEvents {
		testName := inferTestFromRace(race)
		if testName == "" {
			continue
		}
		for _, gid := range []uint64{race.AccessA.GoroutineID, race.AccessB.GoroutineID} {
			if _, exists := gidToTest[gid]; !exists {
				gidToTest[gid] = testName
			}
		}
	}

	// Step 2: assign lanes and collisions to groups
	groups := make(map[string]*TestGroup)
	getOrCreate := func(name string) *TestGroup {
		if g, ok := groups[name]; ok {
			return g
		}
		g := &TestGroup{TestName: name}
		groups[name] = g
		return g
	}

	for _, lane := range tl.Lanes {
		testName := gidToTest[lane.ID]
		if testName == "" {
			testName = "other"
		}
		g := getOrCreate(testName)
		g.Lanes = append(g.Lanes, lane)
	}

	for _, c := range tl.Collisions {
		// Assign collision to the group of goroutine A (primary accessor)
		// Fall back to goroutine B if A is unattributed
		testName := gidToTest[c.GoroutineA]
		if testName == "" {
			testName = gidToTest[c.GoroutineB]
		}
		if testName == "" {
			testName = "other"
		}
		g := getOrCreate(testName)
		g.Collisions = append(g.Collisions, c)
		g.RaceEventIDs = appendUniq(g.RaceEventIDs, c.RaceEventID)
	}

	// Step 3: sort and finalize groups
	var names []string
	for name := range groups {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		// "other" always last; Test* functions alphabetically
		a, b := names[i], names[j]
		if a == "other" {
			return false
		}
		if b == "other" {
			return true
		}
		return a < b
	})

	for _, name := range names {
		g := groups[name]

		// Set HasRace scoped to THIS group's collisions only.
		// Critical: a goroutine must not be flagged as "racing" in a safe test
		// just because it shares an ID with a goroutine in a different racy test.
		racingInGroup := make(map[uint64]bool, len(g.Collisions)*2)
		for _, c := range g.Collisions {
			racingInGroup[c.GoroutineA] = true
			racingInGroup[c.GoroutineB] = true
		}
		for i := range g.Lanes {
			g.Lanes[i].HasRace = racingInGroup[g.Lanes[i].ID]
		}

		// Sort: racing goroutines first (most interesting), then by ID
		sort.Slice(g.Lanes, func(i, j int) bool {
			if g.Lanes[i].HasRace != g.Lanes[j].HasRace {
				return g.Lanes[i].HasRace
			}
			return g.Lanes[i].ID < g.Lanes[j].ID
		})

		tl.Groups = append(tl.Groups, *g)
	}
}

// ---- Helpers ----

// inferTestFromRace extracts a TestXxx function name from a race event's
// creation stacks. Used as fallback when trace data doesn't have creation info.
func inferTestFromRace(race parser.RaceEvent) string {
	for _, frames := range [][]parser.StackFrame{race.CreationA, race.CreationB} {
		for _, f := range frames {
			fn := f.Function
			if dot := strings.LastIndex(fn, "."); dot >= 0 {
				fn = fn[dot+1:]
			}
			if strings.HasPrefix(fn, "Test") {
				return fn
			}
		}
	}
	return ""
}

// collectRelevantGoroutines returns the set of goroutine IDs to include
// in the visualization. Includes:
//   - All goroutines directly involved in race events
//   - All goroutines with a known test name (safe tests need to be visible too)
//
// Excludes:
//   - Runtime internals (GC, timer, sysmon) without a test attribution
//   - Goroutine 0 (invalid)
func collectRelevantGoroutines(races []parser.RaceEvent, tr *parser.TraceResult) map[uint64]bool {
	relevant := make(map[uint64]bool)

	// Always include racing goroutines
	for _, race := range races {
		if race.AccessA.GoroutineID != 0 {
			relevant[race.AccessA.GoroutineID] = true
		}
		if race.AccessB.GoroutineID != 0 {
			relevant[race.AccessB.GoroutineID] = true
		}
	}

	// Include all test-attributed goroutines so safe tests appear in the UI
	// alongside racy ones, giving developers a visual before/after comparison.
	if tr != nil {
		for gid, info := range tr.Goroutines {
			if gid == 0 {
				continue
			}
			if info.Creation != nil && info.Creation.TestName != "" {
				relevant[gid] = true
			}
		}
	}

	return relevant
}

// bestRunningWindow returns the running window for a goroutine most likely
// to contain the race collision — the LAST running window, since the race
// detector fires after the fact (at detection time, not at access time).
//
// Complexity: O(n) single forward pass — earlier O(n²) nested loop version
// was replaced because goroutines in worker pools can have thousands of events.
func bestRunningWindow(gid uint64, tr *parser.TraceResult) (startNs, endNs uint64) {
	info, ok := tr.Goroutines[gid]
	if !ok {
		return tr.MinTime, tr.MaxTime
	}

	// Single forward pass tracking running state transitions.
	// We record each run window as we exit it, keeping only the last one.
	var lastStart, lastEnd uint64
	var inRunning bool
	var runStart uint64

	for _, ev := range info.Events {
		switch {
		case ev.ToState == parser.GoStateRunning:
			// Goroutine entered Running state
			runStart = ev.TimestampNs
			inRunning = true
		case inRunning && ev.FromState == parser.GoStateRunning:
			// Goroutine left Running state — record this completed window
			lastStart = runStart
			lastEnd = ev.TimestampNs
			inRunning = false
		}
	}

	// If goroutine is still running at end of trace, close the window at MaxTime
	if inRunning {
		lastStart = runStart
		lastEnd = tr.MaxTime
	}

	if lastStart > 0 {
		return lastStart, lastEnd
	}
	return info.FirstSeen, info.LastSeen
}

func gLabel(id uint64) string { return fmt.Sprintf("G%d", id) }

func appendUniq(s []int, v int) []int {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func max64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
