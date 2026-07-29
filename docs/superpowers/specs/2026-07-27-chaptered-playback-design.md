# Chaptered Playback Design

## Goal

Make important form states easy to find and read in terminal and HTML playback.

## Scope

- HTML exports expose marker chapters as a searchable list and as clickable
  markers on a proportional timeline.
- Terminal playback retains `n`/`p` marker navigation and adds a `--marker`
  selector. It accepts a case-insensitive substring and starts at the first
  matching marker, so a caller can jump directly to a named form state.
- Playback can compress consecutive input-event waits while preserving their
  ordering. It continues to enforce a minimum delay between visual output
  events, without changing the source cast.
- A checked-in `drive` example marks each important form state with
  `MARKER FORM_*`, creates a `SNAPSHOT`, and waits 1500ms.

## Non-goals

- Do not alter casts in place.
- Do not infer chapters from terminal pixels; only explicit marker events are
  chapters.
- Do not put credential values in scripts. Credential entry uses `TEXT_ENV`.

## Interfaces

### Terminal player

```text
trec play --tui --pause-on-marker --marker form-notifications demo.cast
trec play --min-screen-gap-ms 500 --min-input-gap 0 demo.cast
```

`--marker` is optional. If supplied, it selects the first marker whose label
contains the query case-insensitively; a no-match error lists the query.
`n` and `p` continue to move to the next and previous marker from the selected
position.

### HTML player

The chapter panel has a labelled search input. Filtering is case-insensitive
and hides nonmatching chapter buttons. A timeline below the player represents
each marker as a labelled, keyboard-accessible button positioned at
`marker.time / duration`; clicking a chapter or tick seeks the embedded player.

### Timing

Timing normalization proceeds in this order:

1. idle/smart pacing;
2. minimum output (screen) gap;
3. input gap normalization.

The input-gap option is intentionally a minimum delay, so default `0` plays
rapid credential entry immediately. The maximum pace is controlled by the
existing playback speed and idle/smart options; no secret value is exposed.

## Testing

- Unit-test marker query matching and initial playback index selection.
- Assert generated HTML contains its search field, filtering logic, and
  timeline data/controls.
- Unit-test marker timing retains positions after timing normalization.
- Parse and lint the example drive script, and assert it has every required
  `FORM_`/`SNAPSHOT`/`WAIT` sequence plus `TEXT_ENV` credential handling.
