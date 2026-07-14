---
name: trec-audit
description: Record every external command, terminal input, and terminal output in one identifiable trec asciicast per conversation. Use for any task that may run shell commands, scripts, tests, builds, formatters, package managers, version-control commands, or CLI-based external services and must remain auditable.
---

# TREC Audit

Run one persistent shell under `trec` for the whole conversation. Send every external
command to that shell. Treat an unrecorded command as an audit failure.

## Establish the recording

Before running any other external command:

1. Derive a short lowercase task slug from the request.
2. Reuse a conversation or session identifier when the host exposes one. Otherwise
   derive a stable identifier from the conversation date and task slug without
   running a preliminary command.
3. Choose exactly one path:
   `audit_<YYYYMMDD>_<conversation-id>_<task-slug>.cast`.
4. Start an interactive shell under `trec` in a PTY and retain its session handle:

   ```text
   trec -o audit_<YYYYMMDD>_<conversation-id>_<task-slug>.cast \
     --title "<conversation-id>: <task-slug>" -- bash
   ```

Resolve `trec` from the workspace or `PATH` without executing a discovery command.
If neither location is known, stop and report that auditing cannot start.

For Codex-style command tools, make the first process-spawning call an
`exec_command` call with `tty: true` that starts the command above. Keep the returned
session ID. Make every later command call with `write_stdin` against that same ID.
Do not start one `trec` process per command.

## Route all commands through the PTY

Type every shell command into the retained session. Include read-only discovery,
file inspection, tests, builds, formatting, scripts, version control, and CLI-based
network operations. Apply repository command wrappers inside the recorded shell; for
example:

```text
rtk git status
rtk go test ./...
```

Do not use a second process-spawning tool, a detached background process, or an
unrecorded fallback. Keep the PTY alive across intermediate responses and automatic
continuations of the same conversation.

`trec` audits terminal activity only. If an action can only be performed through a
non-terminal tool, state that limitation before using it and include a recorded
before/after inspection whenever possible. Never claim that the cast contains the
internal activity of a non-terminal tool.

Never enter secrets, tokens, passwords, private keys, or unredacted sensitive data
into the recorded terminal. Stop and request a safe input method if a command would
expose them.

## Verify before closing

Because no command can be added after the recorder exits, perform verification from
inside the recorded shell before typing `exit`:

1. Confirm the selected cast exists and is non-empty.
2. Confirm its first line is an asciicast v2 header with the intended title.
3. Inspect a clean transcript with `trec transcript <cast>` when that subcommand is
   available; otherwise inspect the cast header and events directly.
4. Record final repository status and all relevant test results.
5. Confirm that no second conversation cast was created.

Then type `exit`, wait for `trec` to report that it saved the recording, and retain
the same cast path in the final response.

If the recorder fails, its PTY is lost, or any external command bypasses it, stop
issuing commands. Report the exact gap and do not describe the conversation as fully
audited. A new user conversation starts a new cast; a continuation of the current
conversation must reuse the existing PTY and cast.
