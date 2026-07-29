package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// The official standalone player bundle includes its WASM runtime. Embedding
// it keeps generated pages and trec serve usable without a CDN connection.
//
//go:embed assets/asciinema-player.min.js
var asciinemaPlayerJS []byte

//go:embed assets/asciinema-player.css
var asciinemaPlayerCSS []byte

var annotationPanelJS = template.JS(`
function formatAnnotationTime(seconds) {
  const whole = Math.max(0, Math.floor(Number(seconds) || 0));
  const minutes = Math.floor(whole / 60);
  return String(minutes).padStart(2, "0") + ":" + String(whole % 60).padStart(2, "0");
}
function castDuration(castText) {
  let duration = 0;
  for (const line of castText.split("\n")) {
    try {
      const event = JSON.parse(line);
      if (Array.isArray(event) && Number.isFinite(Number(event[0]))) duration = Math.max(duration, Number(event[0]));
    } catch (_) {}
  }
  return duration;
}
function mountAnnotationPanel(panel, list, search, timeline, encoded, getPlayer, duration) {
  let annotations = [];
  try {
    const parsed = JSON.parse(b64Utf8(encoded));
    if (Array.isArray(parsed)) annotations = parsed;
  } catch (_) {}
  list.replaceChildren();
  panel.hidden = annotations.length === 0;
  timeline.hidden = annotations.length === 0;
  const rows = annotations.map(annotation => {
    const row = document.createElement("button");
    row.type = "button";
    row.className = "annotation-item";
    const time = document.createElement("span");
    time.className = "annotation-time";
    time.textContent = formatAnnotationTime(annotation.time);
    const label = document.createElement("span");
    label.className = "annotation-label";
    label.textContent = String(annotation.label ?? "");
    row.append(time, label);
    row.addEventListener("click", () => {
      const player = getPlayer();
      if (player) player.seek(Number(annotation.time) || 0);
    });
    list.append(row);
    return { row, time: Number(annotation.time) || 0, label: String(annotation.label ?? "") };
  });
  const seek = time => {
    const player = getPlayer();
    if (player) player.seek(time);
  };
  for (const entry of rows) {
    const tick = document.createElement("button");
    tick.type = "button";
    tick.className = "timeline-tick";
    tick.style.left = (duration > 0 ? Math.min(100, Math.max(0, entry.time / duration * 100)) : 0) + "%";
    tick.setAttribute("aria-label", formatAnnotationTime(entry.time) + " " + entry.label);
    tick.title = tick.getAttribute("aria-label");
    tick.addEventListener("click", () => seek(entry.time));
    timeline.append(tick);
  }
  const filter = () => {
    const query = String(search.value || "").trim().toLocaleLowerCase();
    rows.forEach(entry => { entry.row.hidden = query !== "" && !entry.label.toLocaleLowerCase().includes(query); });
  };
  search.addEventListener("input", filter);
  if (rows.length === 0) return () => {};
  let disposed = false;
  const update = async () => {
    const player = getPlayer();
    if (!player || disposed) return;
    const current = Number(await Promise.resolve(player.getCurrentTime()));
    let active = -1;
    rows.forEach((entry, index) => { if (entry.time <= current) active = index; });
    rows.forEach((entry, index) => entry.row.classList.toggle("active", index === active));
  };
  const timer = setInterval(update, 100);
  update();
  return () => { disposed = true; clearInterval(timer); search.removeEventListener("input", filter); };
}
`)

// textViewJS lets the player open everything that was ever printed to the
// terminal as plain text in a new browser tab, without needing a server
// round-trip - the asciicast is already embedded client-side so the
// recording stays shareable as a single offline file. Viewing (rather than
// forcing a download) lets the user just use the browser's own Ctrl+S if
// they want a local copy.
var textViewJS = template.JS(`
const ansiStripRe = /\x1b(?:\[[0-?]*[ -\/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])/g;
function extractPlainText(castText) {
  let out = "";
  for (const line of castText.split("\n")) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    let event;
    try { event = JSON.parse(trimmed); } catch (_) { continue; }
    if (!Array.isArray(event) || event[1] !== "o") continue;
    out += String(event[2] ?? "");
  }
  out = out.replace(ansiStripRe, "");
  out = out.replace(/\r\n?/g, "\n");
  out = out.replace(/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/g, "");
  return out;
}
function downloadText(text, filename) {
  const blob = new Blob([text], { type: "text/plain;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = filename;
  document.body.appendChild(link);
  link.click();
  link.remove();
  URL.revokeObjectURL(url);
}
function viewText(text, filename) {
  const blob = new Blob([text], { type: "text/plain;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const tab = window.open(url, "_blank");
  if (!tab) {
    // Popup blocked: fall back to a direct download so the content isn't lost.
    downloadText(text, filename);
  }
  setTimeout(() => URL.revokeObjectURL(url), 60000);
}
function mountTextViewButton(button, getCastText, filename) {
  if (!button) return;
  button.addEventListener("click", () => viewText(extractPlainText(getCastText()), filename));
}
`)

var unsafeFilenameChars = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`)

func sanitizeFilename(name string) string {
	name = unsafeFilenameChars.ReplaceAllString(name, "_")
	name = strings.TrimSpace(name)
	if name == "" {
		return "recording"
	}
	return name
}

var htmlPageTemplate = template.Must(template.New("player").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}}</title>
  <style>{{.PlayerCSS}}</style>
  <style>
    body { margin: 0; padding: 2rem; background: #111; color: #eee; }
    .recording-layout { display: grid; grid-template-columns: minmax(0, 1fr) minmax(16rem, 24rem); gap: 1.5rem; max-width: 90rem; margin: auto; align-items: start; }
    #player { width: 100%; }
    .marker-timeline { position: relative; height: 1.25rem; margin-top: .6rem; border-radius: .4rem; background: #303030; }
    .marker-timeline::before { content: ""; position: absolute; left: 0; right: 0; top: .55rem; border-top: 1px solid #79849a; }
    .timeline-tick { position: absolute; top: .22rem; width: .55rem; height: .82rem; margin-left: -.275rem; border: 1px solid #b9d1ff; border-radius: .15rem; background: #476da8; cursor: pointer; }
    .timeline-tick:hover, .timeline-tick:focus-visible { background: #91b9ff; outline: 2px solid #fff; }
    .annotation-panel { max-height: min(70vh, 42rem); overflow: auto; border: 1px solid #444; border-radius: .5rem; background: #1b1b1b; }
    .annotation-panel[hidden] { display: none; }
    .annotation-panel h2 { margin: 0; padding: .8rem 1rem; font-size: 1rem; border-bottom: 1px solid #444; }
    .annotation-search { box-sizing: border-box; width: calc(100% - 1rem); margin: .5rem; padding: .55rem; border: 1px solid #555; border-radius: .35rem; background: #111; color: #eee; font: inherit; }
    .annotation-list { display: grid; gap: .35rem; padding: .5rem; }
    .annotation-item { display: grid; grid-template-columns: auto 1fr; gap: .65rem; width: 100%; padding: .65rem; border: 0; border-radius: .35rem; color: #ddd; background: transparent; text-align: left; cursor: pointer; font: inherit; }
    .annotation-item:hover, .annotation-item.active { background: #303b52; color: #fff; }
    .annotation-time { color: #9fb8e8; font-variant-numeric: tabular-nums; white-space: nowrap; }
    .annotation-label { overflow-wrap: anywhere; white-space: pre-wrap; }
    .toolbar { max-width: 90rem; margin: 0 auto 1rem; }
    .view-text-btn { padding: .5rem 1rem; border: 1px solid #444; border-radius: .35rem; background: #1b1b1b; color: #ddd; cursor: pointer; font: inherit; }
    .view-text-btn:hover { background: #303b52; color: #fff; }
    @media (max-width: 900px) { .recording-layout { grid-template-columns: 1fr; } .annotation-panel { max-height: 24rem; } }
  </style>
</head>
<body>
  {{if .HasCompressed}}<label style="display:block;max-width:100%;margin:0 auto 1rem;color:#ddd"><input id="compress" type="checkbox"{{if .CompressedDefault}} checked{{end}}> 壓縮停頓{{if .IdleLimit}}（上限 {{.IdleLimit}} 秒）{{end}}{{if .MinInputGap}}，輸入至少間隔 {{.MinInputGap}} 秒{{end}}</label>{{end}}
  <div class="toolbar">
    <button type="button" id="view-text" class="view-text-btn">查看文字輸出</button>
  </div>
  <div class="recording-layout">
    <div><div id="player"></div><div id="marker-timeline" class="marker-timeline" hidden aria-label="Marker timeline"></div></div>
    <aside id="annotation-panel" class="annotation-panel" hidden aria-label="Annotations">
      <h2>Annotations</h2>
      <input id="annotation-search" class="annotation-search" type="search" placeholder="Search chapters" aria-label="Search chapters">
      <div id="annotation-list" class="annotation-list"></div>
    </aside>
  </div>
  <script>{{.PlayerJS}}</script>
  <script>function b64Utf8(b64) { return new TextDecoder().decode(Uint8Array.from(atob(b64), byte => byte.charCodeAt(0))); }</script>
  <script>{{.AnnotationJS}}</script>
  <script>{{.TextViewJS}}</script>
  <script>
    // The recording is embedded so this HTML can be deployed without its .cast file.
    // atob() only yields a byte-per-character binary string, so every base64 payload
    // (cast bytes AND marker JSON) must go through TextDecoder to become real UTF-8 text -
    // otherwise multi-byte characters (e.g. non-ASCII marker labels) come out mojibake.
    function b64Utf8(b64) {
      return new TextDecoder().decode(Uint8Array.from(atob(b64), byte => byte.charCodeAt(0)));
    }
    const rawCast = b64Utf8("{{.CastBase64}}");
    {{if .HasCompressed}}const compressedCast = b64Utf8("{{.CompressedCastBase64}}");{{end}}
    const rawAnnotations = "{{.RawAnnotationsBase64}}";
    {{if .HasCompressed}}const compressedAnnotations = "{{.AnnotationsBase64}}";{{end}}
    const textFilename = b64Utf8("{{.TextFilenameBase64}}");
    const playerOptions = {
      autoPlay: false,
      preload: true,
      fit: "width",
      {{if .KeystrokeOverlay}}keystrokeOverlay: true{{else}}keystrokeOverlay: false{{end}},
      markers: JSON.parse(b64Utf8("{{.MarkersBase64}}"))
    };
    const player = document.getElementById("player");
    const annotationPanel = document.getElementById("annotation-panel");
    const annotationList = document.getElementById("annotation-list");
    const annotationSearch = document.getElementById("annotation-search");
    const markerTimeline = document.getElementById("marker-timeline");
    let playerInstance;
    let annotationCleanup = () => {};
    let activeCastText = rawCast;
    function loadCast() {
      annotationCleanup();
      if (playerInstance) playerInstance.dispose();
      player.replaceChildren();
      markerTimeline.replaceChildren();
      annotationSearch.value = "";
      const compressed = {{if .HasCompressed}}document.getElementById("compress").checked{{else}}false{{end}};
      activeCastText = compressed ? compressedCast : rawCast;
      playerOptions.markers = JSON.parse(b64Utf8(compressed ? "{{.MarkersBase64}}" : "{{.RawMarkersBase64}}"));
      playerInstance = AsciinemaPlayer.create({ data: activeCastText }, player, playerOptions);
      annotationCleanup = mountAnnotationPanel(annotationPanel, annotationList, annotationSearch, markerTimeline, compressed ? compressedAnnotations : rawAnnotations, () => playerInstance, castDuration(activeCastText));
    }
    {{if .HasCompressed}}document.getElementById("compress").addEventListener("change", loadCast);{{end}}
    loadCast();
    mountTextViewButton(document.getElementById("view-text"), () => activeCastText, textFilename);
  </script>
</body>
</html>
`))

type htmlPageData struct {
	Title                string
	CastBase64           string
	CompressedCastBase64 string
	MarkersBase64        string
	RawMarkersBase64     string
	AnnotationsBase64    string
	RawAnnotationsBase64 string
	TextFilenameBase64   string
	PlayerJS             template.JS
	PlayerCSS            template.CSS
	AnnotationJS         template.JS
	TextViewJS           template.JS
	KeystrokeOverlay     bool
	HasCompressed        bool
	CompressedDefault    bool
	IdleLimit            float64
	MinInputGap          float64
}

func htmlOutputPath(castPath string) string {
	ext := filepath.Ext(castPath)
	if ext == "" {
		return castPath + ".html"
	}
	return castPath[:len(castPath)-len(ext)] + ".html"
}

func htmlPageDataFromCast(path, title string) (htmlPageData, error) {
	return htmlPageDataFromCastWithOptions(path, title, true, 0, 0)
}

func htmlPageDataFromCastWithOptions(path, title string, keystrokeOverlay bool, idleLimit, minInputGap float64) (htmlPageData, error) {
	hdr, events, err := loadCastFile(path)
	if err != nil {
		return htmlPageData{}, err
	}
	cast, err := os.ReadFile(path)
	if err != nil {
		return htmlPageData{}, fmt.Errorf("read %s: %w", path, err)
	}
	if idleLimit < 0 || minInputGap < 0 {
		return htmlPageData{}, fmt.Errorf("timing limits must not be negative")
	}
	compressed := cast
	rawMarkers, err := markerJSON(events)
	if err != nil {
		return htmlPageData{}, err
	}
	rawAnnotations, err := annotationJSON(events)
	if err != nil {
		return htmlPageData{}, err
	}
	compressedMarkers := rawMarkers
	compressedAnnotations := rawAnnotations
	if idleLimit > 0 || minInputGap > 0 {
		adjustPlaybackTiming(events, idleLimit, false, 0, 0, minInputGap)
		compressed, err = marshalCast(hdr, events)
		if err != nil {
			return htmlPageData{}, fmt.Errorf("encode compressed cast: %w", err)
		}
		compressedMarkers, err = markerJSON(events)
		if err != nil {
			return htmlPageData{}, err
		}
		compressedAnnotations, err = annotationJSON(events)
		if err != nil {
			return htmlPageData{}, err
		}
	}
	if title == "" {
		title = hdr.Title
	}
	if title == "" {
		title = filepath.Base(path)
	}
	return htmlPageData{
		Title:                title,
		CastBase64:           base64.StdEncoding.EncodeToString(cast),
		CompressedCastBase64: base64.StdEncoding.EncodeToString(compressed),
		MarkersBase64:        base64.StdEncoding.EncodeToString(compressedMarkers),
		RawMarkersBase64:     base64.StdEncoding.EncodeToString(rawMarkers),
		AnnotationsBase64:    base64.StdEncoding.EncodeToString(compressedAnnotations),
		RawAnnotationsBase64: base64.StdEncoding.EncodeToString(rawAnnotations),
		TextFilenameBase64:   base64.StdEncoding.EncodeToString([]byte(sanitizeFilename(title) + ".txt")),
		PlayerJS:             template.JS(asciinemaPlayerJS),
		PlayerCSS:            template.CSS(asciinemaPlayerCSS),
		AnnotationJS:         annotationPanelJS,
		TextViewJS:           textViewJS,
		KeystrokeOverlay:     keystrokeOverlay,
		HasCompressed:        idleLimit > 0,
		CompressedDefault:    idleLimit > 0,
		IdleLimit:            idleLimit,
		MinInputGap:          minInputGap,
	}, nil
}

func markerJSON(events []castEvent) ([]byte, error) {
	markers := make([][]any, 0)
	for _, event := range events {
		if event.typ == "m" {
			markers = append(markers, []any{event.sec, event.data})
		}
	}
	data, err := json.Marshal(markers)
	if err != nil {
		return nil, fmt.Errorf("encode markers: %w", err)
	}
	return data, nil
}

type annotationData struct {
	Time  float64 `json:"time"`
	Label string  `json:"label"`
}

func annotationJSON(events []castEvent) ([]byte, error) {
	annotations := make([]annotationData, 0)
	for _, event := range events {
		if event.typ == "m" {
			annotations = append(annotations, annotationData{Time: event.sec, Label: event.data})
		}
	}
	data, err := json.Marshal(annotations)
	if err != nil {
		return nil, fmt.Errorf("encode annotations: %w", err)
	}
	return data, nil
}

func shareableHTMLPageData(path, title string, keystrokeOverlay, allowScanFindings bool, idleLimit, minInputGap float64) (htmlPageData, error) {
	if !allowScanFindings {
		findings, err := scanCast(path)
		if err != nil {
			return htmlPageData{}, fmt.Errorf("scan %s: %w", path, err)
		}
		if len(findings) > 0 {
			return htmlPageData{}, fmt.Errorf("refusing to share %s: scan found %d likely unredacted secret(s); rerun with --allow-scan-findings only after review", path, len(findings))
		}
	}
	return htmlPageDataFromCastWithOptions(path, title, keystrokeOverlay, idleLimit, minInputGap)
}

func newHTMLCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "html <file.cast>", Short: "Generate a self-contained HTML player", Args: cobra.ExactArgs(1), Run: runHTML}
	cmd.Flags().StringP("output", "o", "", "output HTML file (default: <cast>.html)")
	cmd.Flags().String("title", "", "page title (default: cast title or filename)")
	cmd.Flags().Bool("allow-scan-findings", false, "allow export when the secret scan reports findings")
	cmd.Flags().Bool("keystroke-overlay", true, "show recorded input in the player")
	cmd.Flags().Float64("idle-time-limit", 0, "compress idle gaps to N seconds (0 = disabled)")
	cmd.Flags().Float64("min-input-gap", 0, "ensure at least N seconds between input events in compressed playback (0 = disabled)")
	return cmd
}

func runHTML(cmd *cobra.Command, files []string) {
	output, _ := cmd.Flags().GetString("output")
	title, _ := cmd.Flags().GetString("title")
	allowScanFindings, _ := cmd.Flags().GetBool("allow-scan-findings")
	keystrokeOverlay, _ := cmd.Flags().GetBool("keystroke-overlay")
	idleLimit, _ := cmd.Flags().GetFloat64("idle-time-limit")
	minInputGap, _ := cmd.Flags().GetFloat64("min-input-gap")
	if len(files) != 1 {
		cmd.Usage()
		os.Exit(1)
	}

	data, err := shareableHTMLPageData(files[0], title, keystrokeOverlay, allowScanFindings, idleLimit, minInputGap)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trec html: %v\n", err)
		os.Exit(1)
	}

	path := output
	if path == "" {
		path = htmlOutputPath(files[0])
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trec html: create %s: %v\n", path, err)
		os.Exit(1)
	}
	defer f.Close()

	if err := htmlPageTemplate.Execute(f, data); err != nil {
		fmt.Fprintf(os.Stderr, "trec html: write %s: %v\n", path, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Generated %s\n", path)
}
