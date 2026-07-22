# Agent Instructions

## Package Manager
Use **Go modules** (`go 1.25.5`):
- `go build -o trec .` — build the binary
- `go run .` — run from source
- `go test ./...` — run the test suite
- `go test -race ./...` — check concurrent PTY/recording paths
- `go vet ./...` — static analysis
- `go mod tidy` — update dependencies

## File-Scoped Commands
| Task | Command |
|------|---------|
| Build binary | `go build -o trec .` |
| Vet one file | `go vet main.go` (or list files) |
| Format file | `gofmt -w file.go` |
| Run subcommand | `go run . render file.cast` |
| Run focused test | `go test ./... -run '^TestName$'` |

## Project Structure
- Single `package main` binary; no subpackages.
- `main.go` — top-level dispatch, default `record` behavior, PTY setup.
- `play.go` — interactive playback with status bar and key controls.
- `drive.go` — scripted TUI driving via keystroke scripts.
- `transcript.go` — ANSI-stripped transcript for agents.
- `render.go` — VT100-emulated screen renderer, including marker-state filtering.
- `markers.go` — marker list/query command and regexp/time filtering.
- `annotate.go` — merges marker events from JSON.
- `cast.go` — asciicast v2 load/save helpers.
- `result.go` — atomic `.result.json` sidecar with final screen, snapshots, and cast integrity metadata.
- `scan.go`, `html.go`, `serve.go` — secret scan and safe sharing/export paths.
- `skills/` — project-specific operating instructions; read the matching `SKILL.md` when its task applies:
  - `trec-tui-drive` — scripted or agent-driven interactive TUI/wizard recording; use `TEXT_IF` for character-submits confirmations and `CHECKLIST_DOWN` only for verified scrolling checklists.
  - `trec-mcp` — persistent MCP terminal sessions, especially when an agent cannot retain a normal stdin session; use `terminal_key` for raw-mode Enter/navigation and `terminal_write` for text or drive DSL lines.
  - `trec-terminal-audit` — read-only review of terminal recording safety, integrity, and sharing readiness.

## Key Conventions
- Subcommands are dispatched manually in `main()` by `os.Args[1]`; flags use `github.com/spf13/pflag` per subcommand.
- Asciicast v2 format: one header JSON line, then `[time, type, data]` event lines.
- Event types: `"o"` output, `"i"` input, `"m"` marker, `"r"` resize. Preserve unknown extension events when reading and rewriting casts.
- Shared types: `castHeader` (main.go), `castEvent` (play.go), `recordingWriter` (redact.go), and `loadCastFile`/`writeCastFile` (cast.go).
- PTY output is mirrored to stdout and recorded; stdin is forwarded to the PTY and recorded as `"i"` events.
- `drive` sets `TERM=xterm-256color` and `CI=1` for deterministic TUI rendering under a non-interactive PTY.
- `record`, `drive`, and MCP `terminal_start` recordings create an `in_progress` `<cast>.result.json`, then replace it with final integrity metadata only after the cast is finalized. Recording starts refuse to overwrite an existing cast or result unless `--force` is explicit. Use the `verify` subcommand or MCP `cast_verify` to gate status, integrity, and secret-scan safety together.
- Terminal result.status distinguishes four terminal values: `success` (child exited 0 and the script ran to completion), `ended` (script invoked `END_SESSION` / `QUIT` — intentional, not a failure, `termination.disposition=script_ended` / `interactive_quit`), `failed` (step failure or non-zero child exit), `aborted` (external signal — `termination.disposition=external_signal`). `verify` accepts both `success` and `ended` as valid completions; the rest are issues. Long-running applies keep `status=in_progress` while a 30-second heartbeat goroutine refreshes `last_step.heartbeat_at` and `progress.last_output_age_ms`; `verify` reports `progress.phase=heartbeat_stale` if the heartbeat goes more than 5 minutes without an update, distinguishing "still running" from "looks stuck".
- For long-running apply / multiple identical words in the same session, use `EXPECT_FRESH` / `EXPECT_FRESH_REGEX` instead of `EXPECT` after a `WAIT <ms>` — viewport scrollback can keep the previous run's text on screen and make a plain `EXPECT` match the wrong event. The `WAIT <ms>; EXPECT <generic>` anti-pattern is flagged by `trec drive lint --strict`.
- Result sidecars carry `inputs` (inventory / vault / cwd path + mtime + SHA-256) so `verify` can flag a cast whose inputs are older than the current environment. Do not treat a cast as evidence for the current state without checking `inputs_drift`.
- Completed recordings append a `SESSION_END` marker. Drive results retain the script SHA-256, redacted normalized steps, last-step progress, and update timestamp; `verify` reports unmatched step markers.
- Agent-authored scripts should pass `trec drive lint --strict <script>` and use `ENTER_IF`/`CHOOSE` for screen-conditioned actions. Inline comments begin with a whitespace-delimited `#`; use JSON steps for literal text containing ` #`.
- Development builds expose their VCS revision and dirty state in `version`, cast headers, and result metadata; do not treat a bare `dev` string as sufficient provenance.
- `html` and `serve` refuse casts with secret-scan findings by default; bypassing this requires the explicit `--allow-scan-findings` flag after review.
- `.gitignore` ignores `trec`, `terminal-record`, and `*.cast`.

## Testing
Tests are colocated as `*_test.go`. Run focused tests while iterating; before handoff run `go test ./...`, `go vet ./...`, and `go build -o trec .`. Run `go test -race ./...` when changing PTY, goroutine, recorder, resize, or terminal-session behavior.
