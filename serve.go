package main

import (
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

var castIndexTemplate = template.Must(template.New("cast-index").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
  <title>trec recordings</title>
  <style>{{.PlayerCSS}}</style>
  <style>
    body { max-width: 90rem; margin: 3rem auto; padding: 0 1rem; font-family: system-ui; background: #111; color: #eee; }
    a { color: #8ab4f8; }
    .recording { margin: 2.5rem 0; }
    .recording-header { display: flex; align-items: center; flex-wrap: wrap; gap: 1rem; margin: 0 0 .8rem; }
    .recording-header h2 { margin: 0; font-size: 1.1rem; font-weight: 600; }
    .recording-header label { white-space: nowrap; }
    .recording-layout { display: grid; grid-template-columns: minmax(0, 1fr) minmax(16rem, 24rem); gap: 1.5rem; align-items: start; }
    .player { width: 100%; }
    .annotation-panel { max-height: min(70vh, 42rem); overflow: auto; border: 1px solid #444; border-radius: .5rem; background: #1b1b1b; }
    .annotation-panel[hidden] { display: none; }
    .annotation-panel h3 { margin: 0; padding: .8rem 1rem; font-size: 1rem; border-bottom: 1px solid #444; }
    .annotation-list { display: grid; gap: .35rem; padding: .5rem; }
    .annotation-item { display: grid; grid-template-columns: auto 1fr; gap: .65rem; width: 100%; padding: .65rem; border: 0; border-radius: .35rem; color: #ddd; background: transparent; text-align: left; cursor: pointer; font: inherit; }
    .annotation-item:hover, .annotation-item.active { background: #303b52; color: #fff; }
    .annotation-time { color: #9fb8e8; font-variant-numeric: tabular-nums; white-space: nowrap; }
    .annotation-label { overflow-wrap: anywhere; white-space: pre-wrap; }
    .view-text-btn { padding: .5rem 1rem; border: 1px solid #444; border-radius: .35rem; background: #1b1b1b; color: #ddd; cursor: pointer; font: inherit; }
    .view-text-btn:hover { background: #303b52; color: #fff; }
    @media (max-width: 900px) { .recording-layout { grid-template-columns: 1fr; } .annotation-panel { max-height: 24rem; } }
  </style>
</head>
<body>
  <h1>trec recordings</h1>
  {{if .HasFiles}}{{if .Casts}}
  <script>{{.PlayerJS}}</script>
  <script>function b64Utf8(b64) { return new TextDecoder().decode(Uint8Array.from(atob(b64), byte => byte.charCodeAt(0))); }</script>
  <script>{{.AnnotationJS}}</script>
  <script>{{.TextViewJS}}</script>
  {{range .Casts}}
  <article class="recording">
    <div class="recording-header">
      <h2><a href="/play/{{.URL}}">{{.Name}}</a></h2>
      {{if .HasCompressed}}<label><input id="compress-{{.ID}}" type="checkbox" checked> 壓縮長時間停頓（上限 {{.IdleLimit}} 秒）</label>{{end}}
      <button type="button" id="view-text-{{.ID}}" class="view-text-btn">查看文字輸出</button>
    </div>
    <div class="recording-layout">
      <div class="player" id="player-{{.ID}}"></div>
      <aside id="annotation-panel-{{.ID}}" class="annotation-panel" hidden aria-label="Annotations">
        <h3>Annotations</h3>
        <div id="annotation-list-{{.ID}}" class="annotation-list"></div>
      </aside>
    </div>
    <script>(()=>{
      const rawCast=b64Utf8("{{.CastBase64}}");
      {{if .HasCompressed}}const compressedCast=b64Utf8("{{.CompressedCastBase64}}");{{end}}
      const rawAnnotations="{{.RawAnnotationsBase64}}";
      {{if .HasCompressed}}const compressedAnnotations="{{.AnnotationsBase64}}";{{end}}
      const textFilename=b64Utf8("{{.TextFilenameBase64}}");
      const target=document.getElementById("player-{{.ID}}");
      const panel=document.getElementById("annotation-panel-{{.ID}}");
      const list=document.getElementById("annotation-list-{{.ID}}");
      let instance;
      let annotationCleanup=()=>{};
      let activeCastText=rawCast;
      function load(){
        annotationCleanup();
        if(instance)instance.dispose();
        target.replaceChildren();
        const compressed={{if .HasCompressed}}document.getElementById("compress-{{.ID}}").checked{{else}}false{{end}};
        activeCastText=compressed?compressedCast:rawCast;
        const options={autoPlay:false,preload:true,fit:"width",{{if .KeystrokeOverlay}}keystrokeOverlay:true{{else}}keystrokeOverlay:false{{end}},markers:JSON.parse(b64Utf8(compressed?"{{.MarkersBase64}}":"{{.RawMarkersBase64}}"))};
        instance=AsciinemaPlayer.create({data:activeCastText},target,options);
        annotationCleanup=mountAnnotationPanel(panel,list,compressed?compressedAnnotations:rawAnnotations,()=>instance);
      }
      {{if .HasCompressed}}document.getElementById("compress-{{.ID}}").addEventListener("change",load);{{end}}
      load();
      mountTextViewButton(document.getElementById("view-text-{{.ID}}"),()=>activeCastText,textFilename);
    })();</script>
  </article>
  {{end}}{{end}}{{range .Invalid}}<article class="recording"><h2>{{.Name}}</h2><p>Unable to load this recording: {{.Error}}</p></article>{{end}}{{else}}<p>No .cast files in this directory.</p>{{end}}
</body>
</html>`))

type castLink struct {
	Name string
	URL  string
}

type castOverviewItem struct {
	castLink
	ID int
	htmlPageData
}

type castOverviewData struct {
	Casts        []castOverviewItem
	Invalid      []invalidCast
	HasFiles     bool
	PlayerJS     template.JS
	PlayerCSS    template.CSS
	AnnotationJS template.JS
	TextViewJS   template.JS
}

type invalidCast struct {
	Name  string
	Error string
}

func listCastFiles(dir string) ([]castLink, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	links := make([]castLink, 0)
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.EqualFold(filepath.Ext(entry.Name()), ".cast") {
			links = append(links, castLink{Name: entry.Name(), URL: url.PathEscape(entry.Name())})
		}
	}
	sort.Slice(links, func(i, j int) bool { return links[i].Name < links[j].Name })
	return links, nil
}

func castPath(dir, escapedName string) (string, error) {
	name, err := url.PathUnescape(escapedName)
	if err != nil || name == "" || filepath.Base(name) != name || !strings.EqualFold(filepath.Ext(name), ".cast") {
		return "", fmt.Errorf("invalid cast name")
	}
	return filepath.Join(dir, name), nil
}

func overviewDataFromCasts(dir string) (castOverviewData, error) {
	return overviewDataFromCastsWithOptions(dir, false, true, 0, 0)
}

func overviewDataFromCastsWithOptions(dir string, allowScanFindings, keystrokeOverlay bool, idleLimit, minInputGap float64) (castOverviewData, error) {
	links, err := listCastFiles(dir)
	if err != nil {
		return castOverviewData{}, err
	}
	overview := castOverviewData{
		Casts:        make([]castOverviewItem, 0, len(links)),
		Invalid:      make([]invalidCast, 0),
		HasFiles:     len(links) > 0,
		PlayerJS:     template.JS(asciinemaPlayerJS),
		PlayerCSS:    template.CSS(asciinemaPlayerCSS),
		AnnotationJS: annotationPanelJS,
		TextViewJS:   textViewJS,
	}
	for _, link := range links {
		data, err := shareableHTMLPageData(filepath.Join(dir, link.Name), "", keystrokeOverlay, allowScanFindings, idleLimit, minInputGap)
		if err != nil {
			overview.Invalid = append(overview.Invalid, invalidCast{Name: link.Name, Error: err.Error()})
			continue
		}
		overview.Casts = append(overview.Casts, castOverviewItem{
			castLink:     link,
			ID:           len(overview.Casts),
			htmlPageData: data,
		})
	}
	return overview, nil
}

func newCastServer(dir string) http.Handler {
	return newCastServerWithOptions(dir, false, true, 0, 0)
}

func newCastServerWithOptions(dir string, allowScanFindings, keystrokeOverlay bool, idleLimit, minInputGap float64) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		overview, err := overviewDataFromCastsWithOptions(dir, allowScanFindings, keystrokeOverlay, idleLimit, minInputGap)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := castIndexTemplate.Execute(w, overview); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("/play/", func(w http.ResponseWriter, r *http.Request) {
		path, err := castPath(dir, strings.TrimPrefix(r.URL.Path, "/play/"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		data, err := shareableHTMLPageData(path, "", keystrokeOverlay, allowScanFindings, idleLimit, minInputGap)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := htmlPageTemplate.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	return mux
}

func newServeCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "serve [directory]", Short: "Serve recordings in a web player", Args: cobra.MaximumNArgs(1), Run: runServe}
	cmd.Flags().String("host", "127.0.0.1", "host address to listen on (use 0.0.0.0 for all interfaces)")
	cmd.Flags().IntP("port", "p", 8080, "TCP port to listen on")
	cmd.Flags().Bool("allow-scan-findings", false, "serve recordings even when the secret scan reports findings")
	cmd.Flags().Bool("keystroke-overlay", true, "show recorded input in the player")
	cmd.Flags().Float64("idle-time-limit", 0.8, "offer web playback with idle gaps capped at N seconds (0 = no compression option)")
	cmd.Flags().Float64("min-input-gap", 0, "ensure at least N seconds between input events in compressed playback (0 = disabled)")
	return cmd
}

func runServe(cmd *cobra.Command, dirs []string) {
	host, _ := cmd.Flags().GetString("host")
	port, _ := cmd.Flags().GetInt("port")
	allowScanFindings, _ := cmd.Flags().GetBool("allow-scan-findings")
	keystrokeOverlay, _ := cmd.Flags().GetBool("keystroke-overlay")
	idleLimit, _ := cmd.Flags().GetFloat64("idle-time-limit")
	minInputGap, _ := cmd.Flags().GetFloat64("min-input-gap")
	if len(dirs) > 1 || port < 1 || port > 65535 || idleLimit < 0 || minInputGap < 0 {
		cmd.Usage()
		os.Exit(1)
	}
	dir := "."
	if len(dirs) == 1 {
		dir = dirs[0]
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trec serve: %v\n", err)
		os.Exit(1)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "trec serve: %s is not a directory\n", dir)
		os.Exit(1)
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	fmt.Fprintf(os.Stderr, "Serving %s at http://%s\n", dir, addr)
	if err := http.ListenAndServe(addr, newCastServerWithOptions(dir, allowScanFindings, keystrokeOverlay, idleLimit, minInputGap)); err != nil {
		fmt.Fprintf(os.Stderr, "trec serve: %v\n", err)
		os.Exit(1)
	}
}
