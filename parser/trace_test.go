package parser

import "testing"

func TestBuildCreation_DoesNotPromoteNonTestFrameToTestName(t *testing.T) {
	ci := buildCreation(
		[]stackFrame{{fn: "database/sql.(*Rows).initContextClose", file: "/go/src/database/sql/sql.go", line: 2990}},
		[]stackFrame{
			{fn: "database/sql.(*DB).beginDC", file: "/go/src/database/sql/sql.go", line: 1900},
			{fn: "runtime.goexit", file: "/usr/local/go/src/runtime/asm_arm64.s", line: 1223},
		},
		12,
	)

	if ci.TestName != "" {
		t.Fatalf("non-test frame promoted to test name: %q", ci.TestName)
	}
	if ci.BirthFunc != "(*Rows).initContextClose" {
		t.Fatalf("birth func: want %q, got %q", "(*Rows).initContextClose", ci.BirthFunc)
	}
}

func TestBuildCreation_UsesRealTestFrame(t *testing.T) {
	ci := buildCreation(
		[]stackFrame{{fn: "taskqueue/internal/broker.TestBrokerRace.func1", file: "broker_test.go", line: 42}},
		[]stackFrame{
			{fn: "taskqueue/internal/broker.TestBrokerRace", file: "broker_test.go", line: 40},
			{fn: "testing.tRunner", file: "/usr/local/go/src/testing/testing.go", line: 1934},
		},
		9,
	)

	if ci.TestName != "TestBrokerRace" {
		t.Fatalf("test name: want %q, got %q", "TestBrokerRace", ci.TestName)
	}
}

func TestBuildCreation_DoesNotTreatTestMainAsTestGroup(t *testing.T) {
	ci := buildCreation(
		[]stackFrame{{fn: "runtime/trace.Start.func1", file: "trace.go", line: 140}},
		[]stackFrame{
			{fn: "github.com/bramakrishna16/taskqueue/internal/scheduler.TestMain", file: "scheduler_test.go", line: 24},
			{fn: "testing.MainStart", file: "/usr/local/go/src/testing/testing.go", line: 2300},
		},
		1,
	)

	if ci.TestName != "" {
		t.Fatalf("TestMain promoted to test group: %q", ci.TestName)
	}
}

func TestShortName_StripsModulePathBeforePackagePrefix(t *testing.T) {
	tests := map[string]string{
		"github.com/bramakrishna16/taskqueue/internal/scheduler.TestSchedulerRun.func1": "TestSchedulerRun.func1",
		"database/sql.(*DB).beginDC": "(*DB).beginDC",
		"racy.TestCounterRace":       "TestCounterRace",
		"runtime.goexit":             "goexit",
	}

	for input, want := range tests {
		if got := shortName(input); got != want {
			t.Fatalf("shortName(%q): want %q, got %q", input, want, got)
		}
	}
}

func TestTestNameFromFunction(t *testing.T) {
	tests := map[string]string{
		"github.com/bramakrishna16/taskqueue/internal/scheduler.TestReapStaleTasks_RetriesWhenRetriesRemain":       "TestReapStaleTasks_RetriesWhenRetriesRemain",
		"github.com/bramakrishna16/taskqueue/internal/scheduler.TestReapStaleTasks_RetriesWhenRetriesRemain.func1": "TestReapStaleTasks_RetriesWhenRetriesRemain",
		"racy.TestCounterRace":                                  "TestCounterRace",
		"racy.TestCounterRace.func1":                            "TestCounterRace",
		"github.com/bramakrishna16/taskqueue/internal.Test":     "Test",
		"github.com/bramakrishna16/taskqueue/internal.TestMain": "",
		"testing.tRunner":                                       "",
		"database/sql.(*Rows).awaitDone":                        "",
		"runtime/trace.Start.func1":                             "",
		"(*traceMultiplexer).startLocked":                       "",
	}

	for input, want := range tests {
		if got := TestNameFromFunction(input); got != want {
			t.Fatalf("TestNameFromFunction(%q): want %q, got %q", input, want, got)
		}
	}
}
