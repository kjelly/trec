---
name: trec-terminal-audit
description: Record every external command, terminal input, and terminal output in one identifiable trec asciicast per conversation. Use for any task that may run shell commands, scripts, tests, builds, formatters, package managers, version-control commands, or CLI-based external services and must remain auditable.
---

# TREC Terminal Audit

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

Never type a literal secret, token, password, private key, or other sensitive value
into the recorded shell or command argv. Use trec's declared-value redaction when a
known value can appear in the cast: `--secret-env NAME` or `--secret-file NAME=path`
replace that exact value in headers, output, markers, and input events before they are
written. This is opt-in redaction, not password detection; values that were not
declared are recorded verbatim.

For a program that prompts for a credential, use `trec drive` with `TEXT_ENV NAME` or
`TEXT_FILE path`. Those operations send the actual value to the PTY while recording a
`<redacted:NAME>` placeholder for the input event. Do not manually type a credential
through `trec record`: stdin reads can split one value across input events, so exact
value redaction cannot provide the same cross-event guarantee. Before sharing an HTML
export or `serve` page, remember that the keystroke overlay displays the input events
already stored in the cast; it captures no new browser input, but makes any unredacted
recorded input more visible.

## Verify before closing

Because no command can be added after the recorder exits, perform verification from
inside the recorded shell before typing `exit`:

1. Confirm the selected cast exists and is non-empty.
2. Confirm its first line is an asciicast v2 header with the intended title.
3. Inspect a clean transcript with `trec transcript <cast>` when that subcommand is
   available; otherwise inspect the cast header and events directly.
4. Read the cast's adjacent `.result.json`; its `status` must be `success` and its
   `exit_code` must be zero before describing the audited command as successful.
   A non-success result is audit evidence of failure, not a successful recording.
5. Run trec's secret scan against the cast. A finding blocks sharing, HTML export,
   and HTTP serving; re-record with declared exact-value redaction rather than
   editing the cast by hand.
6. Record final repository status and all relevant test results.
7. Confirm that no second conversation cast was created.

Then type `exit`, wait for `trec` to report that it saved the recording, and retain
the same cast path in the final response.

If the recorder fails, its PTY is lost, or any external command bypasses it, stop
issuing commands. Report the exact gap and do not describe the conversation as fully
audited. A new user conversation starts a new cast; a continuation of the current
conversation must reuse the existing PTY and cast.
