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

	"github.com/spf13/pflag"
)

var castIndexTemplate = template.Must(template.New("cast-index").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>trec recordings</title><style>body{max-width:54rem;margin:3rem auto;padding:0 1rem;font-family:system-ui;background:#111;color:#eee}a{color:#8ab4f8}li{margin:.6rem 0}</style>
</head><body><h1>trec recordings</h1>{{if .}}<ul>{{range .}}<li><a href="/play/{{.URL}}">{{.Name}}</a></li>{{end}}</ul>{{else}}<p>No .cast files in this directory.</p>{{end}}</body></html>`))

type castLink struct {
	Name string
	URL  string
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

func newCastServer(dir string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		links, err := listCastFiles(dir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := castIndexTemplate.Execute(w, links); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("/play/", func(w http.ResponseWriter, r *http.Request) {
		path, err := castPath(dir, strings.TrimPrefix(r.URL.Path, "/play/"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		data, err := htmlPageDataFromCast(path, "")
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := htmlPageTemplate.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	return mux
}

func runServe(args []string) {
	flags := pflag.NewFlagSet("serve", pflag.ExitOnError)
	host := flags.String("host", "127.0.0.1", "host address to listen on (use 0.0.0.0 for all interfaces)")
	port := flags.IntP("port", "p", 8080, "TCP port to listen on")
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: trec serve [options] [directory]")
		fmt.Fprintln(os.Stderr, "\nServes .cast files in the current directory by default. It listens only on localhost unless --host is changed.")
		fmt.Fprintln(os.Stderr, "\nOptions:")
		flags.PrintDefaults()
	}
	flags.Parse(args)
	dirs := flags.Args()
	if len(dirs) > 1 || *port < 1 || *port > 65535 {
		flags.Usage()
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
	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	fmt.Fprintf(os.Stderr, "Serving %s at http://%s\n", dir, addr)
	if err := http.ListenAndServe(addr, newCastServer(dir)); err != nil {
		fmt.Fprintf(os.Stderr, "trec serve: %v\n", err)
		os.Exit(1)
	}
}
