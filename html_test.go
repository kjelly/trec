package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharingBlocksScanFindingsUnlessExplicitlyAllowed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unsafe.cast")
	if err := writeCastFile(path, castHeader{Version: 2, Width: 80, Height: 24}, []castEvent{
		{sec: 1, typ: "o", data: "password=definitely-not-safe"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := shareableHTMLPageData(path, "", true, false, 0, 0); err == nil || !strings.Contains(err.Error(), "refusing to share") {
		t.Fatalf("unreviewed cast error = %v, want scan gate", err)
	}
	data, err := shareableHTMLPageData(path, "", false, true, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if data.KeystrokeOverlay {
		t.Fatal("keystroke overlay override was ignored")
	}

	server := newCastServerWithOptions(filepath.Dir(path), false, true, 0, 0)
	req := httptest.NewRequest(http.MethodGet, "/play/unsafe.cast", nil)
	res := httptest.NewRecorder()
	server.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("serve status = %d, want %d", res.Code, http.StatusForbidden)
	}
}

// atob() only yields a byte-per-character binary string; JSON.parse(atob(...))
// on non-ASCII marker text produces mojibake in the browser. Guard against that
// pattern reappearing, and confirm the base64 payload itself round-trips intact.
func TestPlayerMarkersSurviveNonASCIILabels(t *testing.T) {
	label := "Step 1/6 邊緣情況：非 ASCII 解說文字 マーカー"
	path := filepath.Join(t.TempDir(), "chapters.cast")
	if err := writeCastFile(path, castHeader{Version: 2, Width: 80, Height: 24}, []castEvent{
		{sec: 0, typ: "o", data: "hello\r\n"},
		{sec: 1, typ: "m", data: label},
	}); err != nil {
		t.Fatal(err)
	}

	data, err := shareableHTMLPageData(path, "", true, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := base64.StdEncoding.DecodeString(data.MarkersBase64)
	if err != nil {
		t.Fatalf("decode MarkersBase64: %v", err)
	}
	var markers [][]any
	if err := json.Unmarshal(raw, &markers); err != nil {
		t.Fatalf("unmarshal markers: %v", err)
	}
	if len(markers) != 1 || markers[0][1] != label {
		t.Fatalf("markers = %+v, want single marker with label %q", markers, label)
	}

	var buf strings.Builder
	if err := htmlPageTemplate.Execute(&buf, data); err != nil {
		t.Fatal(err)
	}
	page := buf.String()
	if strings.Contains(page, `JSON.parse(atob(`) {
		t.Fatalf("player page decodes markers with bare atob() (mojibake bug), page:\n%s", page)
	}
	if !strings.Contains(page, "b64Utf8") {
		t.Fatalf("player page missing the UTF-8-safe base64 decode helper")
	}
}

func TestPlayerAnnotationPanelRendersAndPreservesContent(t *testing.T) {
	label := "部署說明 <script>alert(1)</script>"
	path := filepath.Join(t.TempDir(), "annotated.cast")
	if err := writeCastFile(path, castHeader{Version: 2, Width: 80, Height: 24}, []castEvent{
		{sec: 0, typ: "o", data: "hello\r\n"},
		{sec: 1.25, typ: "m", data: label},
	}); err != nil {
		t.Fatal(err)
	}

	data, err := shareableHTMLPageData(path, "", true, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	if err := htmlPageTemplate.Execute(&buf, data); err != nil {
		t.Fatal(err)
	}
	page := buf.String()
	if !strings.Contains(page, "annotation-panel") || !strings.Contains(page, "annotation-list") {
		t.Fatalf("player page has no annotation panel: %s", page)
	}
	if !strings.Contains(page, "textContent") || !strings.Contains(page, ".seek(") || !strings.Contains(page, "getCurrentTime") {
		t.Fatalf("player page is missing annotation interaction wiring: %s", page)
	}
	if !strings.Contains(page, "@media") || !strings.Contains(page, "grid-template-columns") {
		t.Fatalf("player page is missing responsive annotation layout: %s", page)
	}
	annotationPayload, err := json.Marshal([]annotationData{{Time: 1.25, Label: label}})
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(annotationPayload)
	if !strings.Contains(page, encoded) {
		t.Fatalf("player page does not embed annotation content payload %q", encoded)
	}
}

func TestPlayerPageHasTextViewButton(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo.cast")
	if err := writeCastFile(path, castHeader{Version: 2, Width: 80, Height: 24}, []castEvent{
		{sec: 0, typ: "o", data: "hello\r\n"},
	}); err != nil {
		t.Fatal(err)
	}

	data, err := shareableHTMLPageData(path, "", true, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	if err := htmlPageTemplate.Execute(&buf, data); err != nil {
		t.Fatal(err)
	}
	page := buf.String()
	if !strings.Contains(page, `id="view-text"`) {
		t.Fatalf("player page is missing the view-text button: %s", page)
	}
	if !strings.Contains(page, "extractPlainText") || !strings.Contains(page, "viewText") || !strings.Contains(page, "mountTextViewButton") || !strings.Contains(page, "window.open") {
		t.Fatalf("player page is missing text view wiring: %s", page)
	}
	wantFilename := base64.StdEncoding.EncodeToString([]byte("demo.cast.txt"))
	if !strings.Contains(page, wantFilename) {
		t.Fatalf("player page does not embed expected text filename payload %q: %s", wantFilename, page)
	}
}

func TestSanitizeFilenameStripsUnsafeCharacters(t *testing.T) {
	got := sanitizeFilename(`a/b\c:d*e?f"g<h>i|j`)
	if strings.ContainsAny(got, `/\:*?"<>|`) {
		t.Fatalf("sanitizeFilename left unsafe characters: %q", got)
	}
	if sanitizeFilename("   ") != "recording" {
		t.Fatalf("sanitizeFilename should fall back to a default for blank input, got %q", sanitizeFilename("   "))
	}
}
