# trec bugs/limitations found while driving `pilot` (2026-07-17, updated 2026-11-18)

Compiled from `~/github/pilot`'s `.agents/skills/pilot-trec-verification/SKILL.md`,
session memory, and (for the 2026-11-18 update) a review of every pilot agent
session transcript that called trec, while re-verifying `pilot edit`/`pilot deploy`
wizards via `trec drive --script`/`trec mcp`. Split by status; only items
attributable to `trec` itself are listed as bugs — pilot/roster/environment issues
found in the same sessions are tracked in the pilot repo instead.

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
3. **SELECT direction heuristic misled by extraneous on-screen text** —
   [#1](https://github.com/kjelly/trec/issues/1), closed. Fixed in `869f65f`:
   `selectLabel` compares full screen snapshots across presses; a press that
   changes nothing on screen reverses the sweep once, and a second no-op press
   fails fast with `not reached after sweeping both directions … the label may
   only match stale or non-selectable screen text`. Two reproductions, same
   mechanism (the label scan cannot tell a selectable row from leftover text):
   diagnostic stderr interleaved into the PTY stream, and a just-exited
   non-alt-screen Program's rows still on screen (`pilot deploy`'s scope-select
   → catalog-select transition). The fix turns a silent wrong selection into an
   error; it does not make such a screen selectable (see open item 8).
4. **`trec mcp`'s `terminal_write` occasionally failed to deliver Enter** to a
   target program launched directly — [#2](https://github.com/kjelly/trec/issues/2),
   closed. Not reproducible against HEAD; hardened with
   `TestMCPTerminalWriteEnterRawModeChild`, the schema now says Enter is `"\r"`
   (not `"\n"`, which is Ctrl+J), and `terminal_write` gained `delay_ms`.
5. **The secret scan flagged `pilot edit`'s `<已設定>` status marker** —
   [#3](https://github.com/kjelly/trec/issues/3). The vault editor renders a set
   key as `ipa_admin_password = <已設定>` followed directly by a cursor escape
   (`\x1b[K` or `\x1b[49G`), so every vault-editor recording failed
   `trec verify` with `inline-secret-assignment` and `safe_to_share=false`.
   `4b12226` recognised the bare `<已設定>` form only; `561e17b` strips complete
   escape sequences from the captured value before the placeholder check. Across
   72 real recordings showing the marker, findings dropped from 499 in 20
   recordings to 3 in 1 (the synthetic `token=real-secret` fixtures in
   `audit_20260812_trec-scanner-status.cast`).
6. **The secret scan flagged repository source that only names a secret
   variable** — `token==str(path)…`, `token = b''`, `token = line.split()[0]`
   rendered in a recording (a diff or a long Ansible run printing source) made
   `trec verify` fail on two consecutive pilot clean-room rounds. Fixed in
   `7eb2c1a`: comparison/match operators, a string-literal prefix followed by its
   quote, and a call chain with only empty argument lists are not values.
   `token = os.environ.get("X")` (a call with a quoted argument) is still
   reported; widen only from a real recording. The same class showed up in
   pilot's group_vars editor, which lists commented-out examples as
   `node_exporter_basic_auth_password = {{ vault_node_exporter_basic_auth_password }}`;
   `63c8866` exempts a Jinja reference to a single variable (reproduced by
   recording `pilot edit` on host-monitoring's example group_vars).

## Open — not filed yet

7. **MCP `terminal_choose`/`terminal_focus`/`terminal_activate` miss Huh menus
   without an explicit `pointer`.** The default pointer regex
   `^\s*(?:❯|▸|›|→|»|>)\s` (`drive.go`) never matches a Huh v2 row, which starts
   with `┃ > `. `trec drive` users pass `--pointer '^\s*┃?\s*>\s'`; MCP callers
   forget it and get `context deadline exceeded` (2026-11-13). Candidate fix:
   allow an optional `┃` border in the default.
8. **Label selection on non-alt-screen TUIs with stale rows.** When the previous
   screen's rows stay in the grid (pilot does not use the alt screen), a label
   can be reached only through rows that are not selectable, and item 3's fix
   now fails with `not reached after sweeping both directions` (2026-11-13, the
   Alertmanager receiver menu under pilot's top menu). One 2026-08-24 session
   also got `not reached after 120 presses` for a label that was on screen and
   unique; not reduced to a repro. Workaround: `terminal_key` one key at a time
   with `terminal_read_screen` after each.
9. **MCP `terminal_key` has no pacing and no single-character keys.** Eight
   `DOWN` calls sent back to back without a read in between were all lost
   (2026-08-24). `terminal_write` has `delay_ms`, `terminal_key` does not, and
   it rejects a printable key such as `y` (`unsupported terminal key "y"`), which
   is what a single-keypress confirm needs; callers must switch to
   `terminal_write`.
10. **`trec mcp` occasionally shows as connected via `claude mcp list` but
    exposes no callable `mcp__trec__*` tools** in a given agent session —
    observed across a few verification rounds, root cause never isolated
    (client/transport-side, not obviously `trec`'s own code).
11. **`drive lint` warns `WAIT_CHILD_EXIT exceeds 30 minutes` on every
    legitimately long apply** (39 times in one five-node session), with no way
    to acknowledge an intended long wait, so the warning stops being read.
12. **The result's `build` block is trec's own version**, and has been mistaken
    for the driven binary's. Verify the driven binary with
    `go version -m <binary>`; a label such as `recorder_build` would help.
13. **Fixed 2026-11-18: `skills/` and the installed copies in
    `~/.agents/skills/` had diverged.** The repo's `trec-tui-drive`/`trec-mcp`
    (2026-07-22) documented `ACTIVATE`/`FOCUS`/`EXPECT_TRANSITION`/`EXPECT_FRESH`;
    the installed copies (2026-08-03) lacked those but added the wizard-exit
    lifecycle (leave via the wizard, poll `running=false`, then `terminal_close`
    and `cast_verify`). The repo now holds the merge, plus the MCP notes from
    items 7–9 and a correction both copies shared (`END_SESSION`/`QUIT` ends as
    `ended`, not `aborted`), and `~/.agents/skills/trec-mcp` and
    `~/.agents/skills/trec-tui-drive` are symlinks to `skills/`, so there is one
    copy to edit.

## Not filed — not `trec`'s own bug, but worth knowing if you hit the symptom

14. **bubbletea/termenv's OSC 11 background-color query hangs ~5s** under a
    bare PTY with nothing answering it (`trec`'s PTY, by design, doesn't
    pretend to be a real terminal emulator). Not something to fix in `trec`
    — the caller-side workaround is setting `CI=1` in the driven program's
    env so `termenv` skips the query.
15. **Keystrokes written with no pacing get silently dropped** by some
    Bubble Tea programs' ANSI key decoder — a `--key-delay`/inter-key pacing
    concern on the caller side, not a `trec` parsing bug (see item 9 for the
    MCP side, where the caller has no pacing option).
16. **Interactive SSH host-key prompts hang forever** under a non-interactive
    `trec drive --script` recording (nothing can answer a `yes/no` TTY
    prompt) — caller-side fix is always passing
    `-o StrictHostKeyChecking=accept-new` on any raw `ssh` call inside a
    driven script.
17. **The installed binary lagged the repo by months.** Until 2026-11-18
    `~/bin/trec` was `dev+d46f3efc860d.dirty` (2026-07-29 plus uncommitted
    changes), so fixes after it (including item 5's first half) never reached
    pilot sessions, every `trec verify` printed `recording was produced by a
    dirty trec build`, and evidence records cited a version nobody could
    rebuild. Install from a clean commit: `go build -o ~/bin/trec .` on a clean
    tree, then check that `trec version` has no `.dirty` suffix.
