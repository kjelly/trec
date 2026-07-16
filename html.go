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
  <div id="player"></div>
  <script>{{.PlayerJS}}</script>
  <script>
    // The recording is embedded so this HTML can be deployed without its .cast file.
    const cast = new TextDecoder().decode(Uint8Array.from(
      atob("{{.CastBase64}}"), byte => byte.charCodeAt(0)
    ));
    AsciinemaPlayer.create({ data: cast }, document.getElementById("player"), {
      autoPlay: false,
      preload: true,
      fit: "width",
      markers: JSON.parse(atob("{{.MarkersBase64}}"))
    });
  </script>
</body>
</html>
`))

type htmlPageData struct {
	Title         string
	CastBase64    string
	MarkersBase64 string
	PlayerJS      template.JS
	PlayerCSS     template.CSS
}

func htmlOutputPath(castPath string) string {
	ext := filepath.Ext(castPath)
	if ext == "" {
		return castPath + ".html"
	}
	return castPath[:len(castPath)-len(ext)] + ".html"
}

func htmlPageDataFromCast(path, title string) (htmlPageData, error) {
	hdr, events, err := loadCastFile(path)
	if err != nil {
		return htmlPageData{}, err
	}
	cast, err := os.ReadFile(path)
	if err != nil {
		return htmlPageData{}, fmt.Errorf("read %s: %w", path, err)
	}
	if title == "" {
		title = hdr.Title
	}
	if title == "" {
		title = filepath.Base(path)
	}
	markers := make([][]any, 0)
	for _, event := range events {
		if event.typ == "m" {
			markers = append(markers, []any{event.sec, event.data})
		}
	}
	markerJSON, err := json.Marshal(markers)
	if err != nil {
		return htmlPageData{}, fmt.Errorf("encode markers: %w", err)
	}
	return htmlPageData{
		Title:         title,
		CastBase64:    base64.StdEncoding.EncodeToString(cast),
		MarkersBase64: base64.StdEncoding.EncodeToString(markerJSON),
		PlayerJS:      template.JS(asciinemaPlayerJS),
		PlayerCSS:     template.CSS(asciinemaPlayerCSS),
	}, nil
}

func newHTMLCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "html <file.cast>", Short: "Generate a self-contained HTML player", Args: cobra.ExactArgs(1), Run: runHTML}
	cmd.Flags().StringP("output", "o", "", "output HTML file (default: <cast>.html)")
	cmd.Flags().String("title", "", "page title (default: cast title or filename)")
	return cmd
}

func runHTML(cmd *cobra.Command, files []string) {
	output, _ := cmd.Flags().GetString("output")
	title, _ := cmd.Flags().GetString("title")
	if len(files) != 1 {
		cmd.Usage()
		os.Exit(1)
	}

	data, err := htmlPageDataFromCast(files[0], title)
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
