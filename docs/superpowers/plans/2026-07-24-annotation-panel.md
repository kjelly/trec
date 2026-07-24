# Annotation Panel Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a responsive, readable annotation panel to `trec html` and `trec serve`, with seek-on-click and playback highlighting.

**Architecture:** Extend `htmlPageData` with raw and compressed annotation JSON, then render a shared native-JavaScript annotation panel beside each player. Keep the existing asciinema marker timeline and use per-player IDs for the `serve` index.

**Tech Stack:** Go 1.25.5, `html/template`, embedded CSS/JavaScript, asciinema player API, Go tests.

## Global Constraints

- Preserve existing uncommitted UTF-8 marker decoding changes in `html.go`, `serve.go`, `html_test.go`, and `serve_test.go`.
- Do not add frontend dependencies or a CDN; generated HTML must remain self-contained.
- Use `textContent` for annotation labels so cast data cannot become injected HTML.
- Keep raw and compressed annotation times synchronized with the selected cast payload.
- Run `gofmt`, `go test ./...`, `go test -race ./...`, `go vet ./...`, and `go build -o trec .` before handoff.

### Task 1: Add annotation data extraction tests

**Files:**
- Modify: `html_test.go`
- Modify: `serve_test.go`

**Interfaces:**
- Consumes: `htmlPageDataFromCastWithOptions`, `overviewDataFromCastsWithOptions`, and template rendering.
- Produces: failing regression tests specifying the annotation JSON and panel hooks.

- [x] **Step 1: Write the failing tests**

  Add tests that create a cast with two annotations, decode the new raw annotation payload, assert time/label order including Unicode and `<script>` text, render both templates, and require the annotation panel class, `textContent`, `seek`, current-time update, and responsive layout markers.

- [x] **Step 2: Run the focused tests**

  Run `go test ./... -run 'Test(Player|CastServer).*Annotation'`.
  Expected: FAIL because the annotation data field and panel markup do not exist yet.

### Task 2: Add shared annotation data to HTML page data

**Files:**
- Modify: `html.go`

**Interfaces:**
- Consumes: marker events from `loadCastFile` and adjusted events from `adjustPlaybackTiming`.
- Produces: `AnnotationsBase64` and `RawAnnotationsBase64` fields on `htmlPageData`, containing JSON arrays of `{time,label}` objects.

- [x] **Step 1: Add the data type and encoder**

  Define a small JSON-facing annotation type and an encoder that filters `m` events in order and marshals their seconds and labels. Return a wrapped error on marshal failure.

- [x] **Step 2: Populate raw and compressed payloads**

  Encode raw annotations before timing adjustment and encode compressed annotations after adjustment, base64-encoding each payload alongside the existing marker payloads.

- [x] **Step 3: Run the focused tests**

  Run `go test ./... -run 'Test(Player|CastServer).*Annotation'`.
  Expected: data extraction tests pass; template hook tests remain failing until Task 3.

### Task 3: Render and wire the responsive annotation panel

**Files:**
- Modify: `html.go`
- Modify: `serve.go`

**Interfaces:**
- Consumes: `AnnotationsBase64` and `RawAnnotationsBase64` from Task 2.
- Produces: a hidden-on-empty annotation panel, responsive two-column layout, click-to-seek, and playback highlighting in both templates.

- [x] **Step 1: Add shared panel CSS and browser helpers**

  Add CSS for the player/panel grid, panel scrolling, annotation rows, active state, and the narrow viewport one-column breakpoint. Add an inline helper that decodes annotation JSON, creates rows using `textContent`, formats seconds, calls `instance.seek(time)` on click, and updates the active row from the player's current time.

- [x] **Step 2: Integrate the single-player HTML template**

  Wrap the player and annotation panel in the grid, select raw or compressed annotation JSON in `loadCast`, and connect the player lifecycle to the helper. Keep the panel absent/hidden when the array is empty.

- [x] **Step 3: Integrate the multi-player serve template**

  Apply the same structure and helper per `castOverviewItem`, using its existing ID suffixes so each recording has isolated controls and state.

- [x] **Step 4: Run focused tests**

  Run `go test ./... -run 'Test(Player|CastServer).*Annotation'`.
  Expected: PASS.

### Task 4: Refactor and verify the complete feature

**Files:**
- Modify: `html.go` and `serve.go` only if formatting or shared helper cleanup is needed.

- [x] **Step 1: Format changed Go files**

  Run `gofmt -w html.go html_test.go serve.go serve_test.go`.

- [x] **Step 2: Run all required verification**

  Run `go test ./...`, `go test -race ./...`, `go vet ./...`, and `go build -o trec .`.
  Expected: every command exits 0.

- [x] **Step 3: Inspect the diff**

  Run `git diff --check` and `git diff --stat`; confirm only the annotation feature plus the pre-existing user changes are present.
