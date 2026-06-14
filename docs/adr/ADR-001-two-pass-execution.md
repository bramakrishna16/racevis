# ADR-001: Two-pass test execution instead of one

**Status:** Accepted  
**Date:** 2026-01

## Context

racevis needs two kinds of data from a test run:
1. Race detector output — which goroutines touched the same memory address
2. Scheduler trace — when each goroutine ran, blocked, and was created

The natural approach is to combine both flags: `go test -race -trace=file ./...`

## Decision

Run two separate `go test` invocations:
- Pass 1: `go test -race -count=1 -timeout=60s ./...`
- Pass 2: `go test -count=1 -timeout=60s -trace=file .`

## Reasoning

The `-race` and `-trace` flags conflict on certain Go versions. The race detector adds shadow memory instrumentation to every memory access. On Go 1.23+, this interferes with the trace collector's internal bookkeeping, producing a corrupt or empty trace file. Running them separately guarantees both outputs are always captured cleanly.

Additionally, `-trace` does not support `./...` (multiple packages) — it errors with `cannot use -trace flag with multiple packages`. Pass 2 therefore targets `.` (the root package only).

## Trade-offs

- Tests run twice, roughly doubling execution time
- Acceptable for a developer tool used interactively (not in a hot loop)
- The two runs may observe different race conditions since timing is non-deterministic — this is inherent to race detection and not a correctness issue
