package main

import (
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
