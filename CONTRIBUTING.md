# Contributing to racevis

## Setup

```bash
git clone https://github.com/bramakrishna16/racevis
cd racevis
go build ./...
```

Install the pre-commit lint hook:

```bash
bash scripts/install-hooks.sh
```

## Running the demo

```bash
go run .
# Open http://localhost:7777
```

## Running tests

```bash
# All unit tests
go test ./...

# Run against a specific package
go test ./parser/... -v

# Run the full race-detection demo
cd testdata/racy && go test -race ./...
```

## Project structure

```
main.go              — orchestration, CLI flags
runner/              — executes go test, go tool trace
parser/              — parses race detector output + scheduler trace
correlator/          — joins both datasets into a Timeline struct
source/              — reads source files, extracts code snippets
server/              — HTTP server: /api/timeline + static UI
ui/index.html        — single-file vanilla JS frontend
testdata/racy/       — demo package with intentional race conditions
docs/                — design document + 15 ADRs
```

## Adding a new race pattern to the demo

1. Add the racy function to `testdata/racy/main.go`
2. Add a test that calls it to `testdata/racy/main_test.go` or `broker_test.go`
3. Add a safe comparison version so developers can see the before/after

## Adding a new parser format

The trace parser in `parser/trace.go` handles two format eras already (Go ≤1.22 and 1.23+) via the `reOld`/`reNew` regex pair with a `reFallback` catch-all. To add a new format:

1. Add a new package-level `regexp.MustCompile` var
2. Add it as a new `if m := reNew2.FindStringSubmatch...` branch in `parseStateTransition`
3. Add a test case in `parser/race_test.go` with sample output

## Coding conventions

- Package-level comments on every file explaining what it does
- Every exported type and function has a godoc comment
- Errors are wrapped with `fmt.Errorf("package: context: %w", err)`
- Logging uses `[package]` prefix: `log.Printf("[runner] ...")`
- Regexes are package-level vars — never compiled inside loops
- Tests use table-driven style where multiple cases apply

## Before submitting a PR

```bash
golangci-lint run ./...
go test ./...
go vet ./...
```
