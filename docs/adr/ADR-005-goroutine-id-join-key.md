# ADR-005: Goroutine ID as join key between race events and trace

**Status:** Accepted  
**Date:** 2026-03

## Context

Race detector output and scheduler trace are two separate data streams. To correlate them — to say "goroutine 11 raced, and here is its scheduler timeline" — we need a stable identifier present in both.

## Decision

Use the goroutine ID (integer) as the join key.

## Reasoning

Both outputs use the same goroutine ID assigned by the Go runtime:
- Race detector: `by goroutine 11:` and `GoID=11`
- Scheduler trace: `GoID=11 NotExist->Runnable`

Alternatives considered:
- **Memory address**: changes on every run, not stable
- **Stack frame file:line**: not unique — many goroutines may share the same call site
- **Function name**: not unique — many goroutines may run the same function

Goroutine IDs are assigned monotonically by `runtime.newproc` and are stable within a single test run. They are reused across runs, but racevis only analyzes one run at a time.

## Trade-offs

- Works perfectly for single-run analysis
- Cannot correlate across multiple runs (not needed)
- Goroutine IDs can be recycled within a very long run (goroutine ID wraps at uint64 max — effectively never in practice)
