# ADR-007: `HasRace` flag scoped per test group, not global

**Status:** Accepted  
**Date:** 2026-03

## Context

Each `GoroutineLane` has a `HasRace bool` field used by the UI to:
- Color the lane label red in the left panel
- Sort racing goroutines to the top of their test group
- Count "N racing" in the test list

Initially `HasRace` was set globally in `annotateLanes` — if a goroutine appeared in any collision anywhere in the run, it was marked `HasRace=true`.

## Problem

With 150+ goroutines across 10+ test functions, goroutine ID ranges from different tests can be misattributed by the creation stack parser. A goroutine from `TestBrokerNoRace_SafeWorkerPool` would inherit `HasRace=true` because a goroutine in `TestBrokerRace_WorkerPool` was assigned to the wrong group, polluting the safe test with race markers.

The result: `TestBrokerNoRace_SafeWorkerPool` showed "1 racing" in the left panel and displayed red collision zones — despite having no races.

## Decision

Set `HasRace` per-group in `buildGroups`, not globally in `annotateLanes`:

```go
racingInGroup := make(map[uint64]bool, len(g.Collisions)*2)
for _, c := range g.Collisions {
    racingInGroup[c.GoroutineA] = true
    racingInGroup[c.GoroutineB] = true
}
for i := range g.Lanes {
    g.Lanes[i].HasRace = racingInGroup[g.Lanes[i].ID]
}
```

A lane is `HasRace=true` only if a collision from its own group references its goroutine ID.

## Trade-offs

- One extra map allocation per group (negligible)
- `annotateLanes` still populates `CollisionIDs` globally — needed for ECG rendering which uses all collision zones for the `czByGID` map
- The collision zone rendering in the UI is also scoped per group to prevent red zones appearing in wrong test timelines
