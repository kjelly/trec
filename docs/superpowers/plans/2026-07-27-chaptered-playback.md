# Chaptered Playback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make marked form screens searchable and directly reachable while preserving readable visual pacing.

**Architecture:** Reuse cast marker events as the common chapter data source. `play.go` resolves an optional marker query before playback, while the self-contained HTML page renders the same markers as searchable chapter controls and timeline ticks. Timing remains a copy-on-playback normalization layer.

**Tech Stack:** Go 1.25.5, Cobra/pflag, embedded HTML/JavaScript, drive DSL.

## Global Constraints

- Never mutate source casts.
- Marker search is case-insensitive substring matching.
- Credential examples use `TEXT_ENV`, never literal values.
- Form chapters use `MARKER FORM_*`, `SNAPSHOT`, and a 1000–2000ms `WAIT`.

---

### Task 1: Direct marker selection

**Files:**
- Modify: `play.go`, `play_test.go`

- [ ] Add failing tests for a case-insensitive marker search and no-match path.
- [ ] Run `go test ./... -run TestFindMarker` and verify it fails.
- [ ] Add the minimal `--marker` wiring and playback start index helper.
- [ ] Re-run the focused test and verify it passes.

### Task 2: Searchable HTML chapters and marker timeline

**Files:**
- Modify: `html.go`, `html_test.go`

- [ ] Add failing assertions for accessible chapter search and timeline ticks.
- [ ] Run `go test ./... -run TestHTML` and verify it fails.
- [ ] Render the search input, filter chapter buttons in JavaScript, and render proportional clickable marker ticks.
- [ ] Re-run the focused test and verify it passes.

### Task 3: Form walkthrough script

**Files:**
- Create: `examples/form-walkthrough.steps`
- Modify: `drive_test.go`

- [ ] Add a failing test that parses and validates form chapter triplets.
- [ ] Run `go test ./... -run TestFormWalkthroughScript` and verify it fails.
- [ ] Add the safe `TEXT_ENV` walkthrough example with chapter triplets.
- [ ] Re-run the focused test and verify it passes.

### Task 4: Verification

- [ ] Run `gofmt -w play.go play_test.go html.go html_test.go drive_test.go`.
- [ ] Run `go test ./...`, `go vet ./...`, and `go build -o trec .`.
- [ ] Run `trec drive lint --strict examples/form-walkthrough.steps`.
