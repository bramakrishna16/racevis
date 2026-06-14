package parser

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

type GoState string

const (
	GoStateRunning      GoState = "running"
	GoStateRunnable     GoState = "runnable"
	GoStateWaiting      GoState = "waiting"
	GoStateSyscall      GoState = "syscall"
	GoStateNotExist     GoState = "notexist"
	GoStateUndetermined GoState = "undetermined"
)

type SchedulerEvent struct {
	TimestampNs uint64  `json:"timestampNs"`
	GoroutineID uint64  `json:"goroutineId"`
	FromState   GoState `json:"fromState"`
	ToState     GoState `json:"toState"`
	Reason      string  `json:"reason,omitempty"`
	ProcID      int     `json:"procId"`
}

type CreationInfo struct {
	BirthFunc  string `json:"birthFunc"`
	BirthFile  string `json:"birthFile"`
	BirthLine  int    `json:"birthLine"`
	TestName   string `json:"testName"`
	CreatedByG uint64 `json:"createdByG"`
}

type stackFrame struct {
	fn   string
	file string
	line int
}

type GoroutineInfo struct {
	ID        uint64           `json:"id"`
	Events    []SchedulerEvent `json:"events"`
	FirstSeen uint64           `json:"firstSeenNs"`
	LastSeen  uint64           `json:"lastSeenNs"`
	Creation  *CreationInfo    `json:"creation,omitempty"`
}

type TraceResult struct {
	Events     []SchedulerEvent          `json:"events"`
	Goroutines map[uint64]*GoroutineInfo `json:"goroutines"`
	MinTime    uint64                    `json:"minTimeNs"`
	MaxTime    uint64                    `json:"maxTimeNs"`
}

var (
	reOld = regexp.MustCompile(
		`^M=(-?\d+) P=(-?\d+) G=(-?\d+) StateTransition Time=(\d+) Resource=Goroutine\(\d+\) Reason="([^"]*)" GoID=(\d+) (\w+)->(\w+)`,
	)
	reNew = regexp.MustCompile(
		`^M=(-?\d+) P=(-?\d+) G=(-?\d+) StateTransition Time=(\d+) GoID=(\d+) (\w+)->(\w+) Reason="([^"]*)"`,
	)
	reFallback = regexp.MustCompile(
		`StateTransition Time=(\d+).*GoID=(\d+).*?(\w+)->(\w+)`,
	)
	// Single tab = function name:  "\tpackage.Func @ 0xaddr"
	reFunc = regexp.MustCompile(`^\t(\S+) @`)
	// Double tab = file:line:      "\t\t/path/file.go:42"
	reFile = regexp.MustCompile(`^\t\t(\S+\.go):(\d+)`)
	// reContextG extracts the spawner goroutine ID from a StateTransition line.
	// Declared here (not inline) to avoid recompiling on every birth event.
	reContextG = regexp.MustCompile(`\bG=(-?\d+)\b`)
)

// block tracks what multi-line section we're reading
type block int

const (
	bNormal     block = iota
	bTransition       // reading TransitionStack= lines
	bStack            // reading Stack= lines
)

func ParseTraceOutput(r io.Reader) (*TraceResult, error) {
	result := &TraceResult{
		Goroutines: make(map[uint64]*GoroutineInfo),
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)

	cur := bNormal
	var pendingGID uint64
	var pendingByG uint64
	var pendingTs uint64
	var transFrames []stackFrame
	var stackFrames []stackFrame
	var lastFn string
	var lastFile string
	var lastLine int

	saveFrame := func(frames *[]stackFrame) {
		if lastFn != "" {
			*frames = append(*frames, stackFrame{fn: lastFn, file: lastFile, line: lastLine})
			lastFn, lastFile, lastLine = "", "", 0
		}
	}

	commitCreation := func() {
		if pendingGID == 0 {
			return
		}
		saveFrame(&stackFrames)
		g, ok := result.Goroutines[pendingGID]
		if !ok {
			g = &GoroutineInfo{ID: pendingGID, FirstSeen: pendingTs}
			result.Goroutines[pendingGID] = g
		}
		g.Creation = buildCreation(transFrames, stackFrames, pendingByG)
		pendingGID, pendingByG, pendingTs = 0, 0, 0
		transFrames, stackFrames = nil, nil
		lastFn, lastFile, lastLine = "", "", 0
	}

	for scanner.Scan() {
		line := scanner.Text()

		// ── inside a stack block ──────────────────────────────────────
		if cur != bNormal {
			switch {
			case line == "":
				// blank line ends the current stack section
				if cur == bTransition {
					saveFrame(&transFrames)
				} else {
					commitCreation()
					cur = bNormal
				}
				continue

			case line == "Stack=":
				saveFrame(&transFrames)
				cur = bStack
				continue

			case line == "TransitionStack=":
				continue

			case strings.HasPrefix(line, "M="):
				// new event line with no blank line separator — commit and fall through
				commitCreation()
				cur = bNormal
				// fall through to process this M= line

			default:
				if m := reFunc.FindStringSubmatch(line); m != nil {
					// new function — save previous
					if cur == bTransition {
						saveFrame(&transFrames)
					} else {
						saveFrame(&stackFrames)
					}
					lastFn = m[1]
				} else if m := reFile.FindStringSubmatch(line); m != nil {
					lastFile = m[1]
					lastLine, _ = strconv.Atoi(m[2])
					if cur == bTransition {
						saveFrame(&transFrames)
					} else {
						saveFrame(&stackFrames)
					}
				}
				continue
			}
		}

		// ── normal: look for StateTransition with goroutine ──────────
		if !strings.Contains(line, "StateTransition") || !strings.Contains(line, "GoID=") {
			continue
		}

		ev, err := parseStateTransition(line)
		if err != nil {
			continue
		}

		// Record event
		result.Events = append(result.Events, ev)
		g, ok := result.Goroutines[ev.GoroutineID]
		if !ok {
			g = &GoroutineInfo{ID: ev.GoroutineID, FirstSeen: ev.TimestampNs}
			result.Goroutines[ev.GoroutineID] = g
		}
		g.Events = append(g.Events, ev)
		g.LastSeen = ev.TimestampNs

		if result.MinTime == 0 || ev.TimestampNs < result.MinTime {
			result.MinTime = ev.TimestampNs
		}
		if ev.TimestampNs > result.MaxTime {
			result.MaxTime = ev.TimestampNs
		}

		// Birth event → start collecting creation stacks
		if ev.FromState == GoStateNotExist && ev.ToState == GoStateRunnable {
			commitCreation() // flush any previous
			pendingGID = ev.GoroutineID
			pendingTs = ev.TimestampNs
			// Context G= is the spawner goroutine.
			// reContextG is a package-level compiled regex — not compiled here.
			if ms := reContextG.FindAllStringSubmatch(line, -1); len(ms) > 0 {
				v, _ := strconv.ParseInt(ms[0][1], 10, 64)
				if v > 0 {
					pendingByG = uint64(v)
				}
			}
			cur = bTransition
		}
	}

	commitCreation()

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning trace output: %w", err)
	}

	return result, nil
}

func buildCreation(trans, stack []stackFrame, createdByG uint64) *CreationInfo {
	ci := &CreationInfo{CreatedByG: createdByG}

	// BirthFunc = goroutine's own entry (TransitionStack first frame)
	if len(trans) > 0 {
		ci.BirthFunc = shortName(trans[0].fn)
		ci.BirthFile = trans[0].file
		ci.BirthLine = trans[0].line
	}

	// TestName = first TestXxx frame in the spawner Stack
	for _, f := range stack {
		name := shortName(f.fn)
		if strings.HasPrefix(name, "Test") && !strings.HasPrefix(f.fn, "testing.") {
			ci.TestName = name
			break
		}
	}

	// Fallback: first non-runtime, non-testing frame
	if ci.TestName == "" {
		for _, f := range stack {
			if !strings.HasPrefix(f.fn, "runtime.") && !strings.HasPrefix(f.fn, "testing.") && f.fn != "" {
				ci.TestName = shortName(f.fn)
				break
			}
		}
	}

	return ci
}

// shortName strips the package prefix: "racy.TestCounterRace" → "TestCounterRace"
func shortName(fn string) string {
	// Strip only the package prefix: "racy.RunCounterRace.func1" -> "RunCounterRace.func1"
	if i := strings.Index(fn, "."); i >= 0 {
		return fn[i+1:]
	}
	return fn
}

func parseStateTransition(line string) (SchedulerEvent, error) {
	var ev SchedulerEvent

	if m := reOld.FindStringSubmatch(line); m != nil {
		ts, _ := strconv.ParseUint(m[4], 10, 64)
		gid, _ := strconv.ParseUint(m[6], 10, 64)
		p, _ := strconv.Atoi(m[2])
		ev.TimestampNs = ts
		ev.GoroutineID = gid
		ev.ProcID = p
		ev.Reason = m[5]
		ev.FromState = norm(m[7])
		ev.ToState = norm(m[8])
		return ev, nil
	}
	if m := reNew.FindStringSubmatch(line); m != nil {
		ts, _ := strconv.ParseUint(m[4], 10, 64)
		gid, _ := strconv.ParseUint(m[5], 10, 64)
		p, _ := strconv.Atoi(m[2])
		ev.TimestampNs = ts
		ev.GoroutineID = gid
		ev.ProcID = p
		ev.FromState = norm(m[6])
		ev.ToState = norm(m[7])
		ev.Reason = m[8]
		return ev, nil
	}
	if m := reFallback.FindStringSubmatch(line); m != nil {
		ts, _ := strconv.ParseUint(m[1], 10, 64)
		gid, _ := strconv.ParseUint(m[2], 10, 64)
		ev.TimestampNs = ts
		ev.GoroutineID = gid
		ev.FromState = norm(m[3])
		ev.ToState = norm(m[4])
		return ev, nil
	}
	return ev, fmt.Errorf("no pattern matched")
}

func norm(s string) GoState {
	switch strings.ToLower(s) {
	case "running":
		return GoStateRunning
	case "runnable":
		return GoStateRunnable
	case "waiting":
		return GoStateWaiting
	case "syscall":
		return GoStateSyscall
	case "notexist":
		return GoStateNotExist
	default:
		return GoState(strings.ToLower(s))
	}
}
