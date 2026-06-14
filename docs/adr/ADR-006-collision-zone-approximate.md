# ADR-006: Collision zone placement is approximate

**Status:** Accepted  
**Date:** 2026-03

## Context

The race detector reports that a race happened and which goroutines were involved, but does not report when. It uses a happens-before vector clock algorithm that fires after the conflicting accesses are detected — not at the moment of the first access.

## Decision

Place collision zones at the overlap of both goroutines' last running windows. When no overlap exists, use goroutine B's start timestamp as an approximation, with a 5% synthetic width.

```go
func bestRunningWindow(gid uint64, tr *parser.TraceResult) (startNs, endNs uint64) {
    // Returns the LAST running window — most likely to contain the collision
    // since the race detector fires near the end of the problematic access sequence
}
```

## Reasoning

The race detector gives zero timing information. The scheduler trace gives precise timestamps for when each goroutine was in Running state. The best approximation is the intersection of both goroutines' most recent running windows — "most recent" because the race detector fires after the fact, so the collision was near the end of execution, not the beginning.

Getting exact collision timestamps would require modifying the Go race detector (ThreadSanitizer) to emit timestamps — a change to the Go toolchain itself, far out of scope.

## Trade-offs

- Collision zone positions on the timeline are not exact
- They indicate "approximately when the race happened" not "exactly at this nanosecond"
- This is documented in the UI header text: "Red zone = moment two goroutines touched the same memory address simultaneously" (approximately)
- For the tool's purpose — helping developers understand which goroutines raced and in what context — approximate placement is sufficient

## Log output

The correlator logs when it falls back to approximation:
```
[correlator] race #1: G8 and G15 have no overlapping running window — using approximation
```
