# Annotation Panel Design

## Goal

Make annotation content easy to read in both `trec html` and `trec serve`, while preserving the existing timeline markers.

## User experience

Each recording gets an annotation panel beside its player when the viewport is wide enough. The panel lists annotations in timeline order, with a readable timestamp and the complete annotation text. Clicking an item seeks the associated player to that timestamp. As playback advances, the latest reached annotation is highlighted. At smaller widths the panel moves below the player. Recordings without annotations do not render an empty panel.

## Architecture

The existing Go page-data path already extracts marker events and produces separate marker JSON for raw and compressed playback. It will also produce a small annotation JSON payload containing the marker time and label. A shared inline browser helper renders the payload into a safe DOM list and binds it to the existing player instance. The single-page `html` template and the multi-recording `serve` template will use the same CSS/layout and JavaScript behavior, with per-recording element IDs in `serve`.

The browser will use the player API's `seek` and `getCurrentTime` methods. Annotation labels will be assigned with `textContent`, never interpreted as HTML. When compressed playback is selected, the compressed marker times and annotation payload are used so the list remains synchronized with the selected recording timeline.

## Responsive layout

The player and panel are placed in a CSS grid with the player taking the flexible column and the panel using a bounded sidebar width. A media query switches to one column below the breakpoint. The panel is independently scrollable on wide screens so long annotation text does not force the player to become unusably small.

## Error handling and compatibility

An empty annotation array causes the panel to remain hidden. Invalid or missing annotation data must not prevent the recording player from loading; the rendering helper treats malformed payloads as an empty list. Existing scan gates, compressed playback, marker timeline behavior, and UTF-8-safe base64 decoding remain unchanged.

## Testing

Go tests will verify annotation extraction and UTF-8/special-character-safe template output for both generated HTML and the `serve` index. Template assertions will verify the responsive panel hooks, seek/update wiring, and empty-list behavior. Existing full test, race, vet, and build commands remain the handoff gate.
