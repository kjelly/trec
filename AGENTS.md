# Agent Instructions

## Package Manager
Use **Go modules** (`go 1.24`):
- `go build -o trec .` — build the binary
- `go run .` — run from source
- `go vet ./...` — static analysis
- `go mod tidy` — update dependencies

## File-Scoped Commands
| Task | Command |
|------|---------|
| Build binary | `go build -o trec .` |
| Vet one file | `go vet main.go` (or list files) |
| Format file | `gofmt -w file.go` |
| Run subcommand | `go run . play file.cast` |

## Project Structure
- Single `package main` binary; no subpackages.
- `main.go` — top-level dispatch, default `record` behavior, PTY setup.
- `play.go` — interactive playback with status bar and key controls.
- `drive.go` — scripted TUI driving via keystroke scripts.
- `transcript.go` — ANSI-stripped transcript for agents.
- `annotate.go` — merges marker events from JSON.
- `cast.go` — asciicast v2 load/save helpers.
- `skills/trec-terminal-audit/` — project skill for terminal audit recordings. See `SKILL.md` there.

## Key Conventions
- Subcommands are dispatched manually in `main()` by `os.Args[1]`; flags use `github.com/spf13/pflag` per subcommand.
- Asciicast v2 format: one header JSON line, then `[time, type, data]` event lines.
- Event types: `"o"` output, `"i"` input, `"m"` marker.
- Shared types: `castHeader` (main.go), `castEvent` (play.go), helpers `writeEvent` (main.go), `loadCastFile`/`writeCastFile` (cast.go).
- PTY output is mirrored to stdout and recorded; stdin is forwarded to the PTY and recorded as `"i"` events.
- `drive` sets `TERM=xterm-256color` and `CI=1` for deterministic TUI rendering under a non-interactive PTY.
- `.gitignore` ignores `trec`, `terminal-record`, and `*.cast`.

## Testing
No test files exist. Verify with `go vet ./...` and `go build -o trec .` after changes.
