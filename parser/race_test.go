package parser

import (
	"strings"
	"testing"
)

// sampleRaceOutput is representative go test -race output.
// It contains two races: a read/write and a write/write.
const sampleRaceOutput = `
==================
WARNING: DATA RACE
Write at 0x00c000112198 by goroutine 11:
  racy.(*UnsafeCounter).Increment()
      /home/user/racy/main.go:17 +0x2c
  racy.RunCounterRace.func1()
      /home/user/racy/main.go:29 +0x1a

Previous read at 0x00c000112198 by goroutine 7:
  racy.(*UnsafeCounter).Increment()
      /home/user/racy/main.go:17 +0x1e
  racy.RunCounterRace.func1()
      /home/user/racy/main.go:29 +0x1a

Goroutine 11 (running) created at:
  racy.RunCounterRace()
      /home/user/racy/main.go:28 +0x5e
  racy.TestCounterRace()
      /home/user/racy/main_test.go:12 +0x1e

Goroutine 7 (running) created at:
  racy.RunCounterRace()
      /home/user/racy/main.go:28 +0x5e
  racy.TestCounterRace()
      /home/user/racy/main_test.go:12 +0x1e
==================
==================
WARNING: DATA RACE
Write at 0x00c000112198 by goroutine 12:
  racy.(*UnsafeCounter).Increment()
      /home/user/racy/main.go:17 +0x2c

Previous write at 0x00c000112198 by goroutine 11:
  racy.(*UnsafeCounter).Increment()
      /home/user/racy/main.go:17 +0x2c

Goroutine 12 (runnable) created at:
  racy.RunCounterRace()
      /home/user/racy/main.go:28 +0x5e
==================
`

func TestParseRaceOutput_TwoRaces(t *testing.T) {
	events, err := ParseRaceOutput(strings.NewReader(sampleRaceOutput))
	if err != nil {
		t.Fatalf("ParseRaceOutput returned error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 race events, got %d", len(events))
	}
}

func TestParseRaceOutput_RaceIDs(t *testing.T) {
	events, err := ParseRaceOutput(strings.NewReader(sampleRaceOutput))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if events[0].ID != 1 {
		t.Errorf("first event ID: want 1, got %d", events[0].ID)
	}
	if events[1].ID != 2 {
		t.Errorf("second event ID: want 2, got %d", events[1].ID)
	}
}

func TestParseRaceOutput_AccessTypes(t *testing.T) {
	events, _ := ParseRaceOutput(strings.NewReader(sampleRaceOutput))

	// Race 1: write/read
	r1 := events[0]
	if r1.AccessA.Type != AccessWrite {
		t.Errorf("race1 accessA type: want write, got %s", r1.AccessA.Type)
	}
	if r1.AccessB.Type != AccessRead {
		t.Errorf("race1 accessB type: want read, got %s", r1.AccessB.Type)
	}
	if r1.RaceType != "write/read" {
		t.Errorf("race1 raceType: want write/read, got %s", r1.RaceType)
	}

	// Race 2: write/write
	r2 := events[1]
	if r2.RaceType != "write/write" {
		t.Errorf("race2 raceType: want write/write, got %s", r2.RaceType)
	}
}

func TestParseRaceOutput_GoroutineIDs(t *testing.T) {
	events, _ := ParseRaceOutput(strings.NewReader(sampleRaceOutput))
	r1 := events[0]
	if r1.AccessA.GoroutineID != 11 {
		t.Errorf("race1 goroutineA: want 11, got %d", r1.AccessA.GoroutineID)
	}
	if r1.AccessB.GoroutineID != 7 {
		t.Errorf("race1 goroutineB: want 7, got %d", r1.AccessB.GoroutineID)
	}
}

func TestParseRaceOutput_MemoryAddress(t *testing.T) {
	events, _ := ParseRaceOutput(strings.NewReader(sampleRaceOutput))
	want := "0x00c000112198"
	if events[0].Address != want {
		t.Errorf("address: want %s, got %s", want, events[0].Address)
	}
}

func TestParseRaceOutput_StackFrames(t *testing.T) {
	events, _ := ParseRaceOutput(strings.NewReader(sampleRaceOutput))
	stack := events[0].AccessA.Stack
	if len(stack) == 0 {
		t.Fatal("expected stack frames for accessA, got none")
	}
	if stack[0].Line != 17 {
		t.Errorf("stack[0] line: want 17, got %d", stack[0].Line)
	}
	if !strings.Contains(stack[0].File, "main.go") {
		t.Errorf("stack[0] file: want main.go, got %s", stack[0].File)
	}
}

func TestParseRaceOutput_CreationStack(t *testing.T) {
	events, _ := ParseRaceOutput(strings.NewReader(sampleRaceOutput))
	if len(events[0].CreationA) == 0 {
		t.Fatal("expected creation stack for goroutine A, got none")
	}
}

func TestParseRaceOutput_Empty(t *testing.T) {
	events, err := ParseRaceOutput(strings.NewReader(""))
	if err != nil {
		t.Fatalf("unexpected error on empty input: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for empty input, got %d", len(events))
	}
}

func TestParseRaceOutput_NoRaces(t *testing.T) {
	input := "ok  \tracy\t0.123s\n"
	events, err := ParseRaceOutput(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for clean output, got %d", len(events))
	}
}

func TestParseRaceOutput_PartialBlock(t *testing.T) {
	// Race block with no closing === — should still be captured
	input := `==================
WARNING: DATA RACE
Write at 0x00c000112198 by goroutine 11:
  racy.Foo()
      /home/user/main.go:10 +0x1c

Previous read at 0x00c000112198 by goroutine 7:
  racy.Foo()
      /home/user/main.go:10 +0x1c
`
	events, err := ParseRaceOutput(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("expected 1 event from partial block, got %d", len(events))
	}
}
