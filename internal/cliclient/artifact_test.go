package cliclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateArtifactSendsTitleAndMediaType(t *testing.T) {
	var gotTitle, gotContentType, gotTTL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTitle = r.Header.Get("X-Cairn-Title")
		gotContentType = r.Header.Get("Content-Type")
		gotTTL = r.Header.Get("X-Cairn-Ttl-Seconds")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Artifact{ID: "abc12", URL: "https://cairn.sh/abc12"})
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	art, err := c.CreateArtifact(context.Background(), strings.NewReader("hello"), CreateArtifactOptions{
		Title:      "notes.md",
		MediaType:  "text/markdown",
		TTLSeconds: 3600,
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	if art.ID != "abc12" {
		t.Errorf("art.ID = %q", art.ID)
	}
	if gotTitle != "notes.md" {
		t.Errorf("X-Cairn-Title = %q", gotTitle)
	}
	if gotContentType != "text/markdown" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	if gotTTL != "3600" {
		t.Errorf("X-Cairn-Ttl-Seconds = %q, want 3600", gotTTL)
	}
}

func TestCreateArtifactOmitsTTLHeaderWhenUnset(t *testing.T) {
	var sawHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Get("X-Cairn-Ttl-Seconds") != ""
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Artifact{ID: "abc12", URL: "https://cairn.sh/abc12"})
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	if _, err := c.CreateArtifact(context.Background(), strings.NewReader("hi"), CreateArtifactOptions{}); err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	if sawHeader {
		t.Error("X-Cairn-Ttl-Seconds header sent when TTLSeconds was zero (unset)")
	}
}
