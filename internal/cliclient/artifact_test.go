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

// TestCreateArtifactSendsTagsHeader: tags travel as one comma-separated
// X-Cairn-Tags header, in the order given, and decode back from the response.
//
// Governing: ADR-0018, SPEC-0008 REQ "Pipe and Path Ingest"
func TestCreateArtifactSendsTagsHeader(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Values("X-Cairn-Tags")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Artifact{ID: "abc12", URL: "https://cairn.sh/abc12", Tags: []string{"handoff"}})
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	art, err := c.CreateArtifact(context.Background(), strings.NewReader("hi"), CreateArtifactOptions{
		Tags: []string{"handoff", "lane:auto", "issue:stump.wtf/cairn#42"},
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	if len(got) != 1 || got[0] != "handoff,lane:auto,issue:stump.wtf/cairn#42" {
		t.Errorf("X-Cairn-Tags = %q, want one comma-separated header", got)
	}
	if len(art.Tags) != 1 || art.Tags[0] != "handoff" {
		t.Errorf("decoded tags = %q", art.Tags)
	}
}

func TestCreateArtifactOmitsTagsHeaderWhenUnset(t *testing.T) {
	var saw bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, saw = r.Header["X-Cairn-Tags"]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Artifact{ID: "abc12", URL: "https://cairn.sh/abc12"})
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "tok").CreateArtifact(context.Background(), strings.NewReader("hi"), CreateArtifactOptions{}); err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	if saw {
		t.Error("X-Cairn-Tags sent with no tags")
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

// TestCreateArtifactSendsRelationsAndTraceparent: each declared relation
// travels as its own X-Cairn-Relation header line, and the traceparent rides
// the W3C header verbatim; both stay absent when unset.
//
// Governing: SPEC-0024 REQ "Declaring Relations on Create", REQ "Trace
// Context on Create"
func TestCreateArtifactSendsRelationsAndTraceparent(t *testing.T) {
	var rels []string
	var traces []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rels = r.Header.Values("X-Cairn-Relation")
		traces = r.Header.Values("traceparent")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Artifact{ID: "abc12", URL: "https://cairn.sh/abc12"})
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "tok").CreateArtifact(context.Background(), strings.NewReader("hi"), CreateArtifactOptions{
		Relations:   []string{"reply_to=a1b2c3", "follows=d4e5f6"},
		Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	}); err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	if len(rels) != 2 || rels[0] != "reply_to=a1b2c3" || rels[1] != "follows=d4e5f6" {
		t.Errorf("X-Cairn-Relation = %q, want one header per relation", rels)
	}
	if len(traces) != 1 || traces[0] != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
		t.Errorf("traceparent = %q, want the value verbatim", traces)
	}
}
