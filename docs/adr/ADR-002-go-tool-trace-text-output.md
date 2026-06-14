# ADR-002: Parse `go tool trace` text output over binary trace format

**Status:** Accepted  
**Date:** 2026-02

## Context

The runtime trace is written as a binary file (`trace.out`). To extract goroutine scheduling events, we need to parse this binary format.

Two options:
1. Parse the binary format directly
2. Invoke `go tool trace` as a subprocess and parse its text output

## Decision

Use option 2: invoke `go tool trace -d=parsed trace.out` and parse the resulting text stream.

## Reasoning

The binary trace format is implemented in `internal/trace` — a Go internal package. Internal packages cannot be imported from outside the Go standard library. Accessing them would require:

- Copying the internal package into racevis (fragile — breaks on every Go release)
- Using `go:linkname` hacks (unsupported, brittle)
- Forking the Go toolchain (completely impractical)

`go tool trace` already implements the binary parsing and exposes a stable text format via its `-d` flag. This text format has been stable across Go 1.22–1.26 (with one flag name change, handled by ADR-003).

## Trade-offs

- One extra subprocess per analysis run (~10ms overhead)
- Text parsing is more brittle than binary parsing, but the format is stable
- If Go changes the text output format significantly, the parser needs updating — this is a known maintenance burden
- The upside: zero dependency on internal Go packages, full forward compatibility
