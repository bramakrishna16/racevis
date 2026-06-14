# ADR-008: `-trace` targets single package, not `./...`

**Status:** Accepted  
**Date:** 2026-03

## Context

Pass 1 (race detection) uses `./...` to find races across all subpackages. The natural approach for pass 2 (trace) would be the same.

## Decision

Pass 2 targets `.` (the root package only).

## Reasoning

Go does not allow `-trace` with multiple packages and errors explicitly:
```
cannot use -trace flag with multiple packages
```

This is a Go toolchain limitation with no workaround short of running `go test -trace` once per package and merging the traces — which would significantly complicate the implementation.

## Trade-offs

- Trace data only covers goroutines created by the root package's tests
- Goroutines from subpackages appear in the trace only if they're invoked from root package tests
- Acceptable for the vast majority of Go projects which structure tests one-package-per-directory
- For monorepos with deeply nested packages: point `-target` at the specific subpackage directory
