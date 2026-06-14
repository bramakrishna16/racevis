# ADR-003: Try `-d=parsed` first, fall back to `-d=1`

**Status:** Accepted  
**Date:** 2026-02

## Context

Go 1.23 changed the `go tool trace` debug flag from integer mode to named mode:

- Go ≤1.22: `go tool trace -d=1 trace.out`
- Go 1.23+: `go tool trace -d=parsed trace.out`

racevis needs to work on both. The natural approach is to detect the Go version at runtime and pick the right flag.

## Decision

Do not parse the Go version string. Instead, try `-d=parsed` first, check if output was produced, and retry with `-d=1` if the output is empty.

```go
func CollectTraceEvents(traceFile string) ([]byte, error) {
    if out, err := runTraceCmd(traceFile, "-d=parsed"); err == nil && len(out) > 0 {
        return out, nil
    }
    if out, err := runTraceCmd(traceFile, "-d=1"); err == nil && len(out) > 0 {
        return out, nil
    }
    return nil, fmt.Errorf("go tool trace failed with both -d=parsed and -d=1")
}
```

## Reasoning

Version string parsing proved unreliable. On macOS with Homebrew, `go version` output varies (`go version go1.26.4 darwin/arm64`). The version number `1.26` failed to match a regex that expected single-digit minor versions. The try/fallback approach is version-agnostic and handles any future flag changes automatically.

## Trade-offs

- Two subprocess invocations in the worst case (old Go version)
- Adds ~50ms on old Go versions
- More robust than fragile string parsing
- If a future Go version introduces a third mode, it needs to be added to the chain
