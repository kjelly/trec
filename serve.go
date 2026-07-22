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
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>trec recordings</title><style>{{.PlayerCSS}}</style><style>body{max-width:70rem;margin:3rem auto;padding:0 1rem;font-family:system-ui;background:#111;color:#eee}a{color:#8ab4f8}.recording{margin:2.5rem 0}.recording h2{font-size:1.1rem;font-weight:600}.player{max-width:100%;margin:auto}</style>
</head><body><h1>trec recordings</h1>{{if .HasFiles}}{{if .Casts}}<script>{{.PlayerJS}}</script>{{range .Casts}}<article class="recording"><h2><a href="/play/{{.URL}}">{{.Name}}</a></h2>{{if .HasCompressed}}<label><input id="compress-{{.ID}}" type="checkbox" checked> 壓縮長時間停頓（上限 {{.IdleLimit}} 秒）</label>{{end}}<div class="player" id="player-{{.ID}}"></div><script>(()=>{const rawCast=new TextDecoder().decode(Uint8Array.from(atob("{{.CastBase64}}"),byte=>byte.charCodeAt(0)));{{if .HasCompressed}}const compressedCast=new TextDecoder().decode(Uint8Array.from(atob("{{.CompressedCastBase64}}"),byte=>byte.charCodeAt(0)));{{end}}const target=document.getElementById("player-{{.ID}}");let instance;function load(){if(instance)instance.dispose();target.replaceChildren();const compressed={{if .HasCompressed}}document.getElementById("compress-{{.ID}}").checked{{else}}false{{end}};const options={autoPlay:false,preload:true,fit:"width",{{if .KeystrokeOverlay}}keystrokeOverlay:true{{else}}keystrokeOverlay:false{{end}},markers:JSON.parse(atob(compressed?"{{.MarkersBase64}}":"{{.RawMarkersBase64}}"))};instance=AsciinemaPlayer.create({data:compressed?compressedCast:rawCast},target,options)}{{if .HasCompressed}}document.getElementById("compress-{{.ID}}").addEventListener("change",load);{{end}}load();})();</script></article>{{end}}{{end}}{{range .Invalid}}<article class="recording"><h2>{{.Name}}</h2><p>Unable to load this recording: {{.Error}}</p></article>{{end}}{{else}}<p>No .cast files in this directory.</p>{{end}}</body></html>`))

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
	Casts     []castOverviewItem
	Invalid   []invalidCast
	HasFiles  bool
	PlayerJS  template.JS
	PlayerCSS template.CSS
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
		Casts:     make([]castOverviewItem, 0, len(links)),
		Invalid:   make([]invalidCast, 0),
		HasFiles:  len(links) > 0,
		PlayerJS:  template.JS(asciinemaPlayerJS),
		PlayerCSS: template.CSS(asciinemaPlayerCSS),
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
