package main

import (
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
	if index.Code != http.StatusOK || !strings.Contains(indexPage, "href=\"/play/demo.cast\"") || !strings.Contains(indexPage, "href=\"/play/second.cast\"") || !strings.Contains(indexPage, "id=\"player-0\"") || !strings.Contains(indexPage, "id=\"player-1\"") || strings.Count(indexPage, "AsciinemaPlayer.create") != 2 || !strings.Contains(indexPage, "broken.cast") || !strings.Contains(indexPage, "Unable to load this recording") || strings.Contains(indexPage, "notes.txt") {
		t.Fatalf("unexpected index response: status=%d body=%q", index.Code, index.Body.String())
	}

	player := httptest.NewRecorder()
	h.ServeHTTP(player, httptest.NewRequest(http.MethodGet, "/play/demo.cast", nil))
	page := player.Body.String()
	if player.Code != http.StatusOK || !strings.Contains(page, "Uint8Array.from(") || !strings.Contains(page, "AsciinemaPlayer") || strings.Contains(page, "cdn.jsdelivr.net") {
		t.Fatalf("unexpected player response: status=%d body=%q", player.Code, player.Body.String())
	}
}

func TestCastPathRejectsTraversal(t *testing.T) {
	if _, err := castPath("/tmp", "../secret.cast"); err == nil {
		t.Fatal("castPath accepted a traversal path")
	}
}
