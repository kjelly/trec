# Minimum Screen Gap Playback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add `trec play --min-screen-gap-ms` with a 500ms default that spaces consecutive output events without changing cast contents.

**Architecture:** Extend the existing playback timeline normalization in `play.go`. Apply output-event spacing after idle/smart timing and before input spacing; expose the value as milliseconds at the CLI boundary and convert it to seconds internally.

**Tech Stack:** Go 1.25.5, Cobra/pflag, colocated Go tests.

## Global Constraints

- Preserve event order and event payloads.
- `--min-screen-gap-ms` defaults to `500` and `0` disables it.
- Negative values must be rejected before playback starts.
- Do not modify the source `.cast` file.

---

### Task 1: Add failing timing tests

**Files:**
- Modify: `play_test.go`

**Interfaces:**
- Test the new `enforceOutputGap(events []castEvent, minGap float64)` behavior.

- [ ] **Step 1: Write tests for output spacing**

Add tests that require two output events to be at least 0.5 seconds apart, preserve an intervening input event, and leave timestamps unchanged when the gap is zero.

- [ ] **Step 2: Run the focused tests and verify failure**

Run: `go test ./... -run 'TestEnforceOutputGap'`

Expected: FAIL because `enforceOutputGap` does not yet exist.

### Task 2: Implement timeline spacing and CLI wiring

**Files:**
- Modify: `play.go`

**Interfaces:**
- Produce `enforceOutputGap(events []castEvent, minGap float64)`.
- Add `--min-screen-gap-ms` with default `500` to `newPlayCommand`.

- [ ] **Step 1: Implement output-gap normalization**

Track the timestamp of the previous output event and a cumulative shift. For each event, apply the shift first; when an `o` event is too close to the prior `o`, add the missing duration to the shift and current timestamp. Leave non-output events at their shifted position.

- [ ] **Step 2: Connect the option to playback**

Read the integer millisecond flag in `runPlay`, reject values below zero with the existing command error style, convert to seconds, and pass it through `adjustPlaybackTiming` after smart timing and before input-gap enforcement.

- [ ] **Step 3: Run focused tests**

Run: `go test ./... -run 'Test(EnforceOutputGap|AdjustTiming|EnforceInputGap)'`

Expected: PASS.

### Task 3: Verify the complete change

**Files:**
- Modify: `play_test.go`, `play.go`

- [ ] **Step 1: Format and run all tests**

Run: `gofmt -w play.go play_test.go && go test ./...`

Expected: PASS.

- [ ] **Step 2: Run static analysis and build**

Run: `go vet ./... && go build -o trec .`

Expected: both commands succeed.

- [ ] **Step 3: Inspect the final diff**

Run: `git diff --check && git diff -- play.go play_test.go docs/superpowers/specs/2026-07-27-min-screen-gap-design.md docs/superpowers/plans/2026-07-27-min-screen-gap.md`

Expected: only the requested timing, CLI, tests, and documentation changes are present.
