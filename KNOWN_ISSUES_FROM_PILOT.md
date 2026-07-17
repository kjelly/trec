# trec bugs/limitations found while driving `pilot` (2026-07-17)

Compiled from `~/github/pilot`'s `.agents/skills/pilot-trec-verification/SKILL.md`
and session memory while re-verifying `pilot edit`/`pilot deploy` wizards via
`trec drive --script`/`trec mcp`. Split by status; only items attributable to
`trec` itself are listed as bugs — pilot/roster/environment issues found in the
same sessions are tracked in the pilot repo instead.

## Fixed

1. **`DOWN 0` silently behaved as `DOWN 1`** — `atoiOrDef` treated any
   non-positive parsed count as invalid and defaulted to `1` instead of
   erroring or being a no-op. Fixed in commit `6f77bfc` ("refine drive
   controls, session handling, and MCP tests"): replaced with
   `parsePositiveCount`, which now returns a hard parse error
   (`DOWN needs a positive count`, exit 2) before the driven program even
   starts. Verified live against the fixed build.
2. **An MCP tool-schema bug** (command array param missing a `string` items
   declaration) caused at least one session's `trec mcp` connection to fail
   to reconnect mid-session. Fixed in a prior commit (`fix(mcp): declare
   string items for command array schemas`).

## Open — filed as GitHub issues

3. **SELECT direction heuristic can be misled by extraneous on-screen text**
   — filed as [#1](https://github.com/kjelly/trec/issues/1). *Fixed in
   working tree:* `selectLabel` now compares full screen snapshots across
   presses; a press that changes nothing on screen (pointer at a list
   boundary) reverses the sweep direction once instead of re-trusting the
   label scan, and a second no-op press fails fast with
   `not reached after sweeping both directions … the label may only match
   stale or non-selectable screen text`. Regression tests:
   `TestMCPTerminalSelectRecoversFromMisleadingStaleText`,
   `TestMCPTerminalSelectStaleOnlyLabelFailsFast`; also verified live
   against a misleading-header menu via `trec drive --script`. Two distinct
   reproductions, same underlying mechanism (`selectLabel` scans the entire
   currently-rendered screen for any line containing the label, with no way
   to tell "real, currently-selectable row" from "leftover/unrelated text"):
   - Diagnostic stderr (e.g. an app's own debug-dump line) interleaved into
     the same PTY stream can repeat a row's label one line away from the
     real row, sending the direction heuristic the wrong way and pressing
     to a boundary forever.
   - When neither of two sequential Programs uses the alt-screen buffer,
     the *just-exited* Program's leftover rendered content can still be
     sitting in the visible screen grid and get treated as a real row —
     observed driving `pilot deploy`'s scope-select → catalog-select
     transition: `SELECT <first item>` drove the pointer to the *last* row
     instead of the correct first row, with no error.
4. **`trec mcp`'s `terminal_write` occasionally failed to deliver a real
   Enter/carriage-return byte** when launching a target program directly —
   filed as [#2](https://github.com/kjelly/trec/issues/2). One-time
   observation, not yet reduced to a minimal repro; workaround found was
   launching `trec drive --interactive` itself as the `terminal_start`
   command (rather than the target program directly) and driving that
   nested process instead. *Hardened in working tree* per the issue's ask:
   not reproducible against HEAD, so added regression test
   `TestMCPTerminalWriteEnterRawModeChild` (MCP `terminal_write` of `"\r"`
   directly to a raw-mode child must arrive as byte `0d`, no nested `trec
   drive`), documented in the tool schema that Enter is `"\r"` (not
   `"\n"`, which is Ctrl+J), and added an optional `delay_ms` per-character
   pacing parameter to `terminal_write` for TUIs that drop unpaced
   keystrokes.

## Not filed — not `trec`'s own bug, but worth knowing if you hit the symptom

5. **bubbletea/termenv's OSC 11 background-color query hangs ~5s** under a
   bare PTY with nothing answering it (`trec`'s PTY, by design, doesn't
   pretend to be a real terminal emulator). Not something to fix in `trec`
   — the caller-side workaround is setting `CI=1` in the driven program's
   env so `termenv` skips the query.
6. **Keystrokes written with no pacing get silently dropped** by some
   Bubble Tea programs' ANSI key decoder — a `--key-delay`/inter-key pacing
   concern on the caller side, not a `trec` parsing bug.
7. **Interactive SSH host-key prompts hang forever** under a non-interactive
   `trec drive --script` recording (nothing can answer a `yes/no` TTY
   prompt) — caller-side fix is always passing
   `-o StrictHostKeyChecking=accept-new` on any raw `ssh` call inside a
   driven script.
8. **`trec mcp` occasionally shows as connected via `claude mcp list` but
   exposes no callable `mcp__trec__*` tools** in a given agent session —
   observed across a few verification rounds, root cause never isolated
   (client/transport-side, not obviously `trec`'s own code) — recorded here
   only so it's not mistaken for a new/different failure if seen again.
