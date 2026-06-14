# ADR-004: State machine parser for race detector output

**Status:** Accepted  
**Date:** 2026-03

## Context

The race detector writes unstructured text to stdout. A single race block looks like:

```
==================
WARNING: DATA RACE
Write at 0x00c000112198 by goroutine 11:
  racy.(*UnsafeCounter).Increment()
      /home/user/main.go:17 +0x2c

Previous read at 0x00c000112198 by goroutine 7:
  racy.(*UnsafeCounter).Increment()
      /home/user/main.go:17 +0x1e

Goroutine 11 (running) created at:
  racy.RunCounterRace()
      /home/user/main.go:29 +0x5e
==================
```

Stack frames span two lines (function name, then file:line). Multiple races can be interleaved in the output.

## Decision

Parse with an explicit state machine. States:

```
stateIdle → stateAccessA → stateAccessAStack → stateAccessBStack → stateCreationStack → stateCreationBStack
```

A `pendingFunc` variable holds the function name until its file:line pair arrives on the next line.

## Reasoning

A regex-per-line approach works for simple cases but fails on:
- Multi-line stack frames (need to buffer across lines)
- Interleaved races (need to track which block we're in)
- The `created at:` section which uses different formatting than access stacks
- Goroutine state descriptions embedded in parentheses

The state machine handles all of these naturally. Each state knows exactly what format to expect on the next line.

## Trade-offs

- More code than a regex approach (~200 lines vs ~50 lines)
- State machine is harder to modify — adding a new section requires adding a new state
- Correctness justifies the complexity — the regex approach broke on the first real test run
