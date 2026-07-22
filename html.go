package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// The official standalone player bundle includes its WASM runtime. Embedding
// it keeps generated pages and trec serve usable without a CDN connection.
//
//go:embed assets/asciinema-player.min.js
var asciinemaPlayerJS []byte

//go:embed assets/asciinema-player.css
var asciinemaPlayerCSS []byte

var htmlPageTemplate = template.Must(template.New("player").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}}</title>
  <style>{{.PlayerCSS}}</style>
  <style>body { margin: 0; padding: 2rem; background: #111; } #player { max-width: 100%; margin: auto; }</style>
</head>
<body>
  {{if .HasCompressed}}<label style="display:block;max-width:100%;margin:0 auto 1rem;color:#ddd"><input id="compress" type="checkbox"{{if .CompressedDefault}} checked{{end}}> 壓縮停頓{{if .IdleLimit}}（上限 {{.IdleLimit}} 秒）{{end}}{{if .MinInputGap}}，輸入至少間隔 {{.MinInputGap}} 秒{{end}}</label>{{end}}
  <div id="player"></div>
  <script>{{.PlayerJS}}</script>
  <script>
    // The recording is embedded so this HTML can be deployed without its .cast file.
    const rawCast = new TextDecoder().decode(Uint8Array.from(
      atob("{{.CastBase64}}"), byte => byte.charCodeAt(0)
    ));
    {{if .HasCompressed}}const compressedCast = new TextDecoder().decode(Uint8Array.from(
      atob("{{.CompressedCastBase64}}"), byte => byte.charCodeAt(0)
    ));{{end}}
    const playerOptions = {
      autoPlay: false,
      preload: true,
      fit: "width",
      {{if .KeystrokeOverlay}}keystrokeOverlay: true{{else}}keystrokeOverlay: false{{end}},
      markers: JSON.parse(atob("{{.MarkersBase64}}"))
    };
    const player = document.getElementById("player");
    let playerInstance;
    function loadCast() {
      if (playerInstance) playerInstance.dispose();
      player.replaceChildren();
      const compressed = {{if .HasCompressed}}document.getElementById("compress").checked{{else}}false{{end}};
      playerOptions.markers = JSON.parse(atob(compressed ? "{{.MarkersBase64}}" : "{{.RawMarkersBase64}}"));
      playerInstance = AsciinemaPlayer.create({ data: compressed ? compressedCast : rawCast }, player, playerOptions);
    }
    {{if .HasCompressed}}document.getElementById("compress").addEventListener("change", loadCast);{{end}}
    loadCast();
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
	PlayerJS             template.JS
	PlayerCSS            template.CSS
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
	compressedMarkers := rawMarkers
	if idleLimit > 0 || minInputGap > 0 {
		adjustPlaybackTiming(events, idleLimit, false, minInputGap)
		compressed, err = marshalCast(hdr, events)
		if err != nil {
			return htmlPageData{}, fmt.Errorf("encode compressed cast: %w", err)
		}
		compressedMarkers, err = markerJSON(events)
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
		PlayerJS:             template.JS(asciinemaPlayerJS),
		PlayerCSS:            template.CSS(asciinemaPlayerCSS),
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
