# Architecture Decision Records — racevis

ADRs capture the significant technical decisions made during the design and implementation of racevis. Each record explains what was decided, why, and what trade-offs were accepted.

## Index

| ADR | Title | Status |
|-----|-------|--------|
| [ADR-001](ADR-001-two-pass-execution.md) | Two-pass test execution instead of one | Accepted |
| [ADR-002](ADR-002-go-tool-trace-text-output.md) | Parse `go tool trace` text output over binary | Accepted |
| [ADR-003](ADR-003-trace-flag-try-fallback.md) | Try `-d=parsed` first, fall back to `-d=1` | Accepted |
| [ADR-004](ADR-004-state-machine-race-parser.md) | State machine parser for race detector output | Accepted |
| [ADR-005](ADR-005-goroutine-id-join-key.md) | Goroutine ID as join key between race events and trace | Accepted |
| [ADR-006](ADR-006-collision-zone-approximate.md) | Collision zone placement is approximate | Accepted |
| [ADR-007](ADR-007-hasrace-scoped-per-group.md) | `HasRace` flag scoped per test group, not global | Accepted |
| [ADR-008](ADR-008-trace-single-package.md) | `-trace` targets single package not `./...` | Accepted |
| [ADR-009](ADR-009-source-snippet-cache.md) | Source snippets cached by file:line, assigned to all accesses | Accepted |
| [ADR-010](ADR-010-stdlib-source-excluded.md) | Stdlib source files excluded from snippet reading | Accepted |
| [ADR-011](ADR-011-all-test-goroutines-included.md) | All test-attributed goroutines included in visualization | Accepted |
| [ADR-012](ADR-012-annotate-lanes-map-pass.md) | `annotateLanes` uses map pre-pass over nested loops | Accepted |
| [ADR-013](ADR-013-single-file-vanilla-js.md) | Frontend is single-file vanilla JS, no framework | Accepted |
| [ADR-014](ADR-014-svg-ecg-lines.md) | SVG ECG lines instead of div-based bars | Accepted |
| [ADR-015](ADR-015-playhead-reveal-pattern.md) | Playhead reveal pattern for segments | Accepted |
