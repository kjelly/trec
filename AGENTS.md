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
- `skills/` — project-specific operating instructions; read the matching `SKILL.md` when its task applies.

## Key Conventions
- Subcommands are dispatched manually in `main()` by `os.Args[1]`; flags use `github.com/spf13/pflag` per subcommand.
- Asciicast v2 format: one header JSON line, then `[time, type, data]` event lines.
- Event types: `"o"` output, `"i"` input, `"m"` marker, `"r"` resize. Preserve unknown extension events when reading and rewriting casts.
- Shared types: `castHeader` (main.go), `castEvent` (play.go), `recordingWriter` (redact.go), and `loadCastFile`/`writeCastFile` (cast.go).
- PTY output is mirrored to stdout and recorded; stdin is forwarded to the PTY and recorded as `"i"` events.
- `drive` sets `TERM=xterm-256color` and `CI=1` for deterministic TUI rendering under a non-interactive PTY.
- `record` and `drive` write `<cast>.result.json` only after the cast is flushed and synced. Treat `cast.complete`, `cast.sha256`, and `cast.byte_size` as required evidence before trusting a completed recording.
- `html` and `serve` refuse casts with secret-scan findings by default; bypassing this requires the explicit `--allow-scan-findings` flag after review.
- `.gitignore` ignores `trec`, `terminal-record`, and `*.cast`.

## Testing
Tests are colocated as `*_test.go`. Run focused tests while iterating; before handoff run `go test ./...`, `go vet ./...`, and `go build -o trec .`. Run `go test -race ./...` when changing PTY, goroutine, recorder, resize, or terminal-session behavior.
