package correlator

import (
	"github.com/bramakrishna16/racevis/parser"
	"testing"
)

// ---- Helpers ----

func makeRace(id int, gidA, gidB uint64, addr, raceType string) parser.RaceEvent {
	return parser.RaceEvent{
		ID:      id,
		Address: addr,
		AccessA: parser.RaceAccess{
			Type:        parser.AccessWrite,
			GoroutineID: gidA,
			Address:     addr,
		},
		AccessB: parser.RaceAccess{
			Type:        parser.AccessRead,
			GoroutineID: gidB,
			Address:     addr,
		},
		RaceType: raceType,
	}
}

func makeTrace(gids []uint64, minT, maxT uint64) *parser.TraceResult {
	tr := &parser.TraceResult{
		Goroutines: make(map[uint64]*parser.GoroutineInfo),
		MinTime:    minT,
		MaxTime:    maxT,
	}
	for _, gid := range gids {
		midT := (minT + maxT) / 2
		g := &parser.GoroutineInfo{
			ID:        gid,
			FirstSeen: minT,
			LastSeen:  maxT,
			Events: []parser.SchedulerEvent{
				{GoroutineID: gid, TimestampNs: minT, FromState: parser.GoStateNotExist, ToState: parser.GoStateRunnable},
				{GoroutineID: gid, TimestampNs: midT, FromState: parser.GoStateRunnable, ToState: parser.GoStateRunning},
				{GoroutineID: gid, TimestampNs: maxT - 1, FromState: parser.GoStateRunning, ToState: parser.GoStateWaiting},
			},
			Creation: &parser.CreationInfo{TestName: "TestFoo"},
		}
		tr.Goroutines[gid] = g
		tr.Events = append(tr.Events, g.Events...)
	}
	return tr
}

// ---- Tests ----

func TestCorrelate_NoRaces(t *testing.T) {
	tl := Correlate(nil, nil)
	if tl == nil {
		t.Fatal("Correlate returned nil")
	}
	if len(tl.RaceEvents) != 0 {
		t.Errorf("expected 0 race events, got %d", len(tl.RaceEvents))
	}
}

func TestCorrelate_SyntheticWithoutTrace(t *testing.T) {
	races := []parser.RaceEvent{makeRace(1, 7, 8, "0xAAA", "write/read")}
	tl := Correlate(races, nil)

	if tl.HasTraceData {
		t.Error("HasTraceData should be false when no trace provided")
	}
	if len(tl.Lanes) == 0 {
		t.Error("expected synthetic lanes to be built")
	}
	if len(tl.Collisions) != 1 {
		t.Errorf("expected 1 collision, got %d", len(tl.Collisions))
	}
}

func TestCorrelate_WithTrace(t *testing.T) {
	races := []parser.RaceEvent{makeRace(1, 7, 8, "0xAAA", "write/read")}
	tr := makeTrace([]uint64{7, 8}, 1000, 5000)
	tl := Correlate(races, tr)

	if !tl.HasTraceData {
		t.Error("HasTraceData should be true when trace provided")
	}
	if tl.MinTimeNs != 1000 {
		t.Errorf("MinTimeNs: want 1000, got %d", tl.MinTimeNs)
	}
	if tl.MaxTimeNs != 5000 {
		t.Errorf("MaxTimeNs: want 5000, got %d", tl.MaxTimeNs)
	}
	if tl.DurationNs != 4000 {
		t.Errorf("DurationNs: want 4000, got %d", tl.DurationNs)
	}
}

func TestCorrelate_LanesBuilt(t *testing.T) {
	races := []parser.RaceEvent{makeRace(1, 7, 8, "0xAAA", "write/read")}
	tr := makeTrace([]uint64{7, 8}, 1000, 5000)
	tl := Correlate(races, tr)

	if len(tl.Lanes) < 2 {
		t.Errorf("expected at least 2 lanes (G7 and G8), got %d", len(tl.Lanes))
	}
}

func TestCorrelate_CollisionZone(t *testing.T) {
	races := []parser.RaceEvent{makeRace(1, 7, 8, "0xAAA", "write/read")}
	tr := makeTrace([]uint64{7, 8}, 1000, 5000)
	tl := Correlate(races, tr)

	if len(tl.Collisions) != 1 {
		t.Fatalf("expected 1 collision zone, got %d", len(tl.Collisions))
	}
	c := tl.Collisions[0]
	if c.Address != "0xAAA" {
		t.Errorf("collision address: want 0xAAA, got %s", c.Address)
	}
	if c.RaceEventID != 1 {
		t.Errorf("collision raceEventID: want 1, got %d", c.RaceEventID)
	}
	if c.GoroutineA != 7 || c.GoroutineB != 8 {
		t.Errorf("collision goroutines: want G7/G8, got G%d/G%d", c.GoroutineA, c.GoroutineB)
	}
}

func TestCorrelate_HasRaceScopedPerGroup(t *testing.T) {
	// Two races in TestFoo, one goroutine (G9) is safe (belongs to TestBar)
	races := []parser.RaceEvent{makeRace(1, 7, 8, "0xAAA", "write/read")}
	tr := makeTrace([]uint64{7, 8}, 1000, 5000)

	// Add a safe goroutine belonging to a different test
	tr.Goroutines[9] = &parser.GoroutineInfo{
		ID:        9,
		FirstSeen: 1000,
		LastSeen:  5000,
		Events: []parser.SchedulerEvent{
			{GoroutineID: 9, TimestampNs: 1000, FromState: parser.GoStateNotExist, ToState: parser.GoStateRunnable},
		},
		Creation: &parser.CreationInfo{TestName: "TestBar"},
	}

	tl := Correlate(races, tr)

	// Find TestBar group — its goroutine should NOT be marked HasRace
	for _, g := range tl.Groups {
		if g.TestName == "TestBar" {
			for _, lane := range g.Lanes {
				if lane.HasRace {
					t.Errorf("G%d in TestBar is marked HasRace but has no races in its group", lane.ID)
				}
			}
		}
	}
}

func TestCorrelate_MultipleRacesSameAddress(t *testing.T) {
	// Two distinct race events on the same address — should produce 2 collisions
	races := []parser.RaceEvent{
		makeRace(1, 7, 8, "0xAAA", "write/read"),
		makeRace(2, 7, 9, "0xAAA", "write/write"),
	}
	tr := makeTrace([]uint64{7, 8, 9}, 1000, 5000)
	tl := Correlate(races, tr)

	if len(tl.Collisions) != 2 {
		t.Errorf("expected 2 collision zones for 2 races, got %d", len(tl.Collisions))
	}
}

func TestCorrelate_LaneSegmentsPresent(t *testing.T) {
	races := []parser.RaceEvent{makeRace(1, 7, 8, "0xAAA", "write/read")}
	tr := makeTrace([]uint64{7, 8}, 1000, 5000)
	tl := Correlate(races, tr)

	for _, lane := range tl.Lanes {
		if len(lane.Segments) == 0 {
			t.Errorf("lane G%d has no segments — expected scheduler data", lane.ID)
		}
	}
}

func TestCorrelate_GroupsOrganizedByTest(t *testing.T) {
	races := []parser.RaceEvent{makeRace(1, 7, 8, "0xAAA", "write/read")}
	tr := makeTrace([]uint64{7, 8}, 1000, 5000)
	tl := Correlate(races, tr)

	if len(tl.Groups) == 0 {
		t.Fatal("expected at least one test group")
	}
	// All test-attributed goroutines should be in named groups (not "other")
	for _, g := range tl.Groups {
		if g.TestName == "" {
			t.Error("found a group with empty test name")
		}
	}
}

func TestBestRunningWindow_NoEvents(t *testing.T) {
	tr := &parser.TraceResult{
		MinTime: 1000,
		MaxTime: 5000,
		Goroutines: map[uint64]*parser.GoroutineInfo{
			7: {ID: 7, FirstSeen: 1000, LastSeen: 5000, Events: nil},
		},
	}
	start, end := bestRunningWindow(7, tr)
	// Falls back to FirstSeen/LastSeen when no running events
	if start != 1000 || end != 5000 {
		t.Errorf("fallback window: want (1000,5000), got (%d,%d)", start, end)
	}
}

func TestBestRunningWindow_MultipleRunWindows(t *testing.T) {
	// Goroutine runs twice — should return the LAST window
	tr := &parser.TraceResult{
		MinTime: 0,
		MaxTime: 10000,
		Goroutines: map[uint64]*parser.GoroutineInfo{
			7: {
				ID: 7, FirstSeen: 0, LastSeen: 10000,
				Events: []parser.SchedulerEvent{
					{GoroutineID: 7, TimestampNs: 100, FromState: parser.GoStateRunnable, ToState: parser.GoStateRunning},
					{GoroutineID: 7, TimestampNs: 500, FromState: parser.GoStateRunning, ToState: parser.GoStateWaiting},
					{GoroutineID: 7, TimestampNs: 800, FromState: parser.GoStateWaiting, ToState: parser.GoStateRunnable},
					{GoroutineID: 7, TimestampNs: 900, FromState: parser.GoStateRunnable, ToState: parser.GoStateRunning},
					{GoroutineID: 7, TimestampNs: 9000, FromState: parser.GoStateRunning, ToState: parser.GoStateWaiting},
				},
			},
		},
	}
	start, end := bestRunningWindow(7, tr)
	// Last window: 900..9000
	if start != 900 {
		t.Errorf("start: want 900, got %d", start)
	}
	if end != 9000 {
		t.Errorf("end: want 9000, got %d", end)
	}
}

func TestAppendUniq(t *testing.T) {
	s := []int{1, 2, 3}
	s = appendUniq(s, 2) // duplicate — should not add
	if len(s) != 3 {
		t.Errorf("appendUniq added duplicate: want len 3, got %d", len(s))
	}
	s = appendUniq(s, 4) // new value — should add
	if len(s) != 4 {
		t.Errorf("appendUniq missed new value: want len 4, got %d", len(s))
	}
}
