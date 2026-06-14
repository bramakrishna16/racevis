// Package parser handles parsing of go test -race output and runtime/trace binary.
package parser

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// ---- Data types ----

// AccessType is the kind of memory operation involved in a race.
type AccessType string

const (
	AccessRead  AccessType = "read"
	AccessWrite AccessType = "write"
)

// StackFrame is a single frame in a goroutine's call stack.
type StackFrame struct {
	Function string `json:"function"`
	File     string `json:"file"`
	Line     int    `json:"line"`
}

// SnippetLine is one line of source with its metadata.
// Mirrors source.Line — defined here to avoid circular imports between
// parser and source packages.
type SnippetLine struct {
	Number  int    `json:"number"`
	Content string `json:"content"`
	IsRace  bool   `json:"isRace"`
}

// SourceSnippet is a window of source lines around a race site.
// Mirrors source.Snippet — defined here to avoid circular imports.
// Populated by main.go after parsing, before serving.
type SourceSnippet struct {
	File     string        `json:"file"`
	FullFile string        `json:"fullFile"`
	RaceLine int           `json:"raceLine"`
	Lines    []SnippetLine `json:"lines"`
}

// RaceAccess is one side of a detected race — either the "current" or "previous" accessor.
type RaceAccess struct {
	Type        AccessType   `json:"type"`
	Address     string       `json:"address"` // e.g. "0x00c000014208"
	GoroutineID uint64       `json:"goroutineId"`
	Stack       []StackFrame `json:"stack"`
	// Snippet is populated after parsing by main.enrichWithSource.
	// Nil means source was not available (stdlib, missing file, etc.)
	Snippet *SourceSnippet `json:"snippet,omitempty"`
}

// RaceEvent is a single detected data race, as emitted by the Go race detector.
// One "WARNING: DATA RACE" block maps to exactly one RaceEvent.
type RaceEvent struct {
	ID        int          `json:"id"`
	Address   string       `json:"address"`
	AccessA   RaceAccess   `json:"accessA"`   // the triggering access
	AccessB   RaceAccess   `json:"accessB"`   // the previous conflicting access
	CreationA []StackFrame `json:"creationA"` // where goroutine A was created
	CreationB []StackFrame `json:"creationB"` // where goroutine B was created
	RaceType  string       `json:"raceType"`  // e.g. "write/write", "read/write"
	// VarName is the resolved variable name for the racing address.
	// Empty string means DWARF resolution was not attempted or failed.
	// Only populated when racevis is run with the -dwarf flag.
	VarName string `json:"varName,omitempty"`
}

// ---- Parser state machine ----

type parseState int

const (
	stateIdle parseState = iota
	stateAccessA
	stateAccessAStack
	stateAccessB
	stateAccessBStack
	stateCreationHeader
	stateCreationStack
	stateCreationB
	stateCreationBStack
)

var (
	reWarning    = regexp.MustCompile(`^={2,}\s*$`)
	reAccessLine = regexp.MustCompile(`^(Read|Write) at (0x[0-9a-f]+) by goroutine (\d+):`)
	rePrevAccess = regexp.MustCompile(`^Previous (read|write) at (0x[0-9a-f]+) by goroutine (\d+):`)
	reCreation   = regexp.MustCompile(`^Goroutine (\d+) \(.*\) created at:`)
	reFrame      = regexp.MustCompile(`^\s+(\S+)\(`)
	reFileLine   = regexp.MustCompile(`^\s+(.+):(\d+)`)
)

// ParseRaceOutput reads race detector output from r and returns all detected races.
func ParseRaceOutput(r io.Reader) ([]RaceEvent, error) {
	scanner := bufio.NewScanner(r)
	var events []RaceEvent
	var current *RaceEvent
	state := stateIdle
	idCounter := 0

	// pendingFrame holds a function name waiting for its file:line on the next line
	pendingFunc := ""

	flushFrame := func(frames *[]StackFrame, fn, fileLine string, lineNum int) {
		if fn == "" {
			return
		}
		*frames = append(*frames, StackFrame{
			Function: fn,
			File:     fileLine,
			Line:     lineNum,
		})
	}

	for scanner.Scan() {
		line := scanner.Text()

		// A line of "==================" starts or ends a race block
		if reWarning.MatchString(strings.TrimSpace(line)) {
			if current != nil && state != stateIdle {
				// Flush last pending frame
				switch state {
				case stateCreationStack:
					flushFrame(&current.CreationA, pendingFunc, "", 0)
				case stateCreationBStack:
					flushFrame(&current.CreationB, pendingFunc, "", 0)
				}
				finalize(current)
				events = append(events, *current)
				current = nil
				state = stateIdle
				pendingFunc = ""
			}
			continue
		}

		// Start of a race block
		if strings.Contains(line, "WARNING: DATA RACE") {
			idCounter++
			current = &RaceEvent{ID: idCounter}
			state = stateAccessA
			pendingFunc = ""
			continue
		}

		if current == nil {
			continue
		}

		switch state {

		case stateAccessA:
			if m := reAccessLine.FindStringSubmatch(line); m != nil {
				current.AccessA.Type = AccessType(strings.ToLower(m[1]))
				current.AccessA.Address = m[2]
				current.Address = m[2]
				gid, _ := strconv.ParseUint(m[3], 10, 64)
				current.AccessA.GoroutineID = gid
				state = stateAccessAStack
				pendingFunc = ""
			}

		case stateAccessAStack:
			if m := rePrevAccess.FindStringSubmatch(line); m != nil {
				// Flush last pending frame
				flushFrame(&current.AccessA.Stack, pendingFunc, "", 0)
				pendingFunc = ""
				current.AccessB.Type = AccessType(m[1])
				current.AccessB.Address = m[2]
				gid, _ := strconv.ParseUint(m[3], 10, 64)
				current.AccessB.GoroutineID = gid
				state = stateAccessBStack
			} else {
				pendingFunc = parseStackLine(line, &current.AccessA.Stack, pendingFunc)
			}

		case stateAccessBStack:
			if m := reCreation.FindStringSubmatch(line); m != nil {
				flushFrame(&current.AccessB.Stack, pendingFunc, "", 0)
				pendingFunc = ""
				// Which goroutine's creation site is this?
				gid, _ := strconv.ParseUint(m[1], 10, 64)
				if gid == current.AccessA.GoroutineID {
					state = stateCreationStack
				} else {
					state = stateCreationBStack
				}
			} else {
				pendingFunc = parseStackLine(line, &current.AccessB.Stack, pendingFunc)
			}

		case stateCreationStack:
			if m := reCreation.FindStringSubmatch(line); m != nil {
				flushFrame(&current.CreationA, pendingFunc, "", 0)
				pendingFunc = ""
				state = stateCreationBStack
			} else {
				pendingFunc = parseStackLine(line, &current.CreationA, pendingFunc)
			}

		case stateCreationBStack:
			pendingFunc = parseStackLine(line, &current.CreationB, pendingFunc)
		}
	}

	// Handle last event if file ended without trailing "==="
	if current != nil {
		finalize(current)
		events = append(events, *current)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning race output: %w", err)
	}

	return events, nil
}

// parseStackLine handles a single line that may be either a function name or a file:line pair.
// Returns updated pendingFunc.
func parseStackLine(line string, frames *[]StackFrame, pendingFunc string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return pendingFunc
	}

	// File:line pair — e.g. "    /home/user/main.go:42 +0x1c"
	if reFileLine.MatchString(line) {
		m := reFileLine.FindStringSubmatch(line)
		filePath := strings.TrimSpace(m[1])
		// Strip the +0x... offset if present
		if idx := strings.LastIndex(filePath, " "); idx >= 0 {
			filePath = filePath[:idx]
		}
		lineNum, _ := strconv.Atoi(m[2])
		if pendingFunc != "" {
			*frames = append(*frames, StackFrame{
				Function: pendingFunc,
				File:     filePath,
				Line:     lineNum,
			})
			return "" // consumed
		}
		return pendingFunc
	}

	// Function name line — e.g. "  racy.(*UnsafeCounter).Increment()"
	if reFrame.MatchString(line) {
		// Flush previous pending func (no file info available for it)
		if pendingFunc != "" {
			*frames = append(*frames, StackFrame{Function: pendingFunc})
		}
		m := reFrame.FindStringSubmatch(line)
		return m[1] // return as new pending
	}

	return pendingFunc
}

// finalize sets derived fields on a completed RaceEvent.
func finalize(e *RaceEvent) {
	e.RaceType = fmt.Sprintf("%s/%s", e.AccessA.Type, e.AccessB.Type)
	// Normalize address — use AccessA's address as canonical
	if e.Address == "" {
		e.Address = e.AccessA.Address
	}
}
