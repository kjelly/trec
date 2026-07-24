package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCastServerListsAndPlaysCastFiles(t *testing.T) {
	dir := t.TempDir()
	cast := "{\"version\":2,\"width\":80,\"height\":24}\n[0.1,\"o\",\"hello\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "demo.cast"), []byte(cast), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "second.cast"), []byte(cast), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.cast"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a cast"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newCastServer(dir)

	index := httptest.NewRecorder()
	h.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/", nil))
	indexPage := index.Body.String()
	if index.Code != http.StatusOK || !strings.Contains(indexPage, "href=\"/play/demo.cast\"") || !strings.Contains(indexPage, "href=\"/play/second.cast\"") || !strings.Contains(indexPage, "id=\"player-0\"") || !strings.Contains(indexPage, "id=\"player-1\"") || strings.Count(indexPage, "AsciinemaPlayer.create") != 2 || strings.Count(indexPage, "keystrokeOverlay:true") != 2 || !strings.Contains(indexPage, "broken.cast") || !strings.Contains(indexPage, "Unable to load this recording") || strings.Contains(indexPage, "notes.txt") {
		t.Fatalf("unexpected index response: status=%d body=%q", index.Code, index.Body.String())
	}

	player := httptest.NewRecorder()
	h.ServeHTTP(player, httptest.NewRequest(http.MethodGet, "/play/demo.cast", nil))
	page := player.Body.String()
	if player.Code != http.StatusOK || !strings.Contains(page, "Uint8Array.from(") || !strings.Contains(page, "AsciinemaPlayer") || !strings.Contains(page, "keystrokeOverlay: true") || strings.Contains(page, "cdn.jsdelivr.net") {
		t.Fatalf("unexpected player response: status=%d body=%q", player.Code, player.Body.String())
	}
}

func TestCastPathRejectsTraversal(t *testing.T) {
	if _, err := castPath("/tmp", "../secret.cast"); err == nil {
		t.Fatal("castPath accepted a traversal path")
	}
}

// Same mojibake guard as TestPlayerMarkersSurviveNonASCIILabels, for the "/"
// index page's per-cast inline player template in serve.go.
func TestCastServerIndexMarkersSurviveNonASCIILabels(t *testing.T) {
	dir := t.TempDir()
	cast := "{\"version\":2,\"width\":80,\"height\":24}\n[0.1,\"o\",\"hello\"]\n[0.2,\"m\",\"Step 1/6 邊緣情況：非 ASCII 解說文字\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "demo.cast"), []byte(cast), 0o600); err != nil {
		t.Fatal(err)
	}

	h := newCastServer(dir)
	index := httptest.NewRecorder()
	h.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/", nil))
	page := index.Body.String()
	if index.Code != http.StatusOK {
		t.Fatalf("index status = %d, want 200", index.Code)
	}
	if strings.Contains(page, `markers:JSON.parse(atob(`) {
		t.Fatalf("index page decodes markers with bare atob() (mojibake bug), page:\n%s", page)
	}
	if !strings.Contains(page, "b64Utf8") {
		t.Fatalf("index page missing the UTF-8-safe base64 decode helper")
	}
}

func TestCastServerIndexRendersAnnotationPanel(t *testing.T) {
	dir := t.TempDir()
	cast := "{\"version\":2,\"width\":80,\"height\":24}\n[0.1,\"o\",\"hello\"]\n[0.2,\"m\",\"檢查 <b>輸出</b>\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "demo.cast"), []byte(cast), 0o600); err != nil {
		t.Fatal(err)
	}

	server := newCastServer(dir)
	res := httptest.NewRecorder()
	server.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/", nil))
	page := res.Body.String()
	if res.Code != http.StatusOK {
		t.Fatalf("index status = %d, want 200", res.Code)
	}
	if !strings.Contains(page, "annotation-panel") || !strings.Contains(page, "annotation-list") {
		t.Fatalf("index page has no annotation panel: %s", page)
	}
	if !strings.Contains(page, "textContent") || !strings.Contains(page, ".seek(") {
		t.Fatalf("index page is missing annotation interaction wiring: %s", page)
	}
	annotationPayload, err := json.Marshal([]annotationData{{Time: 0.2, Label: "檢查 <b>輸出</b>"}})
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(annotationPayload)
	if !strings.Contains(page, encoded) {
		t.Fatalf("index page does not embed annotation content payload %q", encoded)
	}
}

func TestCastServerIndexHasTextViewButton(t *testing.T) {
	dir := t.TempDir()
	cast := "{\"version\":2,\"width\":80,\"height\":24}\n[0.1,\"o\",\"hello\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "demo.cast"), []byte(cast), 0o600); err != nil {
		t.Fatal(err)
	}

	h := newCastServer(dir)
	index := httptest.NewRecorder()
	h.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/", nil))
	page := index.Body.String()
	if index.Code != http.StatusOK {
		t.Fatalf("index status = %d, want 200", index.Code)
	}
	if !strings.Contains(page, `id="view-text-0"`) {
		t.Fatalf("index page is missing the view-text button: %s", page)
	}
	if !strings.Contains(page, "extractPlainText") || !strings.Contains(page, "viewText") || !strings.Contains(page, "mountTextViewButton") || !strings.Contains(page, "window.open") {
		t.Fatalf("index page is missing text view wiring: %s", page)
	}
	wantFilename := base64.StdEncoding.EncodeToString([]byte("demo.cast.txt"))
	if !strings.Contains(page, wantFilename) {
		t.Fatalf("index page does not embed expected text filename payload %q: %s", wantFilename, page)
	}

	player := httptest.NewRecorder()
	h.ServeHTTP(player, httptest.NewRequest(http.MethodGet, "/play/demo.cast", nil))
	playerPage := player.Body.String()
	if player.Code != http.StatusOK || !strings.Contains(playerPage, `id="view-text"`) {
		t.Fatalf("play page is missing the view-text button: status=%d body=%q", player.Code, playerPage)
	}
}
