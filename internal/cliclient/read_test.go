package cliclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetArtifactDecodesMetadataAndMembers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/artifacts/bundle1" {
			t.Errorf("path = %q, want /v1/artifacts/bundle1", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         "bundle1",
			"url":        "https://cairn.test/bundle1",
			"mcp":        "mcp://cairn/bundle1",
			"share_type": "bundle",
			"visibility": "link",
			"media_type": "multipart/mixed",
			"tags":       []string{"handoff", "lane:m"},
			"members": []any{
				map[string]any{"name": "notes.md", "size": 4, "media_type": "text/markdown"},
				map[string]any{"name": "run.log", "size": 4, "media_type": "text/plain"},
			},
		})
	}))
	defer srv.Close()

	art, err := New(srv.URL, "").GetArtifact(context.Background(), "bundle1")
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if art.ShareType != "bundle" || art.MCP != "mcp://cairn/bundle1" {
		t.Errorf("got share_type=%q mcp=%q", art.ShareType, art.MCP)
	}
	if len(art.Members) != 2 || art.Members[0].Name != "notes.md" || art.Members[0].Size != 4 {
		t.Fatalf("members = %+v, want notes.md(4) first", art.Members)
	}
	if len(art.Tags) != 2 || art.Tags[1] != "lane:m" {
		t.Errorf("tags = %v, want [handoff lane:m]", art.Tags)
	}
}

func TestGetArtifactNotFoundMapsToSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "not_found", "message": "artifact not found"},
		})
	}))
	defer srv.Close()

	_, err := New(srv.URL, "").GetArtifact(context.Background(), "gone")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetArtifact error = %v, want ErrNotFound", err)
	}
}

func TestOpenBodyStreamsBytesAndMeta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/artifacts/abc123/body" {
			t.Errorf("path = %q, want the body route", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Cairn-Checksum", "deadbeef")
		w.Write([]byte("the whole body"))
	}))
	defer srv.Close()

	rc, meta, err := New(srv.URL, "").OpenBody(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("OpenBody: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != "the whole body" {
		t.Fatalf("body = %q", got)
	}
	if meta.SHA256 != "deadbeef" {
		t.Errorf("meta.SHA256 = %q, want deadbeef", meta.SHA256)
	}
}

// A member name is a path, not an opaque token: every segment is escaped so the
// request addresses the member the server stored rather than resolving to a
// different route.
func TestOpenMemberEscapesNameSegments(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.Write([]byte("x"))
	}))
	defer srv.Close()

	// A "#" unescaped would become a URL fragment and silently address a
	// different path; a space is the everyday case.
	rc, _, err := New(srv.URL, "").OpenMember(context.Background(), "abc123", "logs/run #1.log")
	if err != nil {
		t.Fatalf("OpenMember: %v", err)
	}
	rc.Close()

	if gotPath != "/v1/artifacts/abc123/members/logs/run%20%231.log" {
		t.Fatalf("path = %q, want each segment escaped but the separator intact", gotPath)
	}
}

func TestOpenMemberForbiddenMapsToSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "forbidden", "message": "forbidden"},
		})
	}))
	defer srv.Close()

	_, _, err := New(srv.URL, "tok").OpenMember(context.Background(), "p", "a.txt")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("OpenMember error = %v, want ErrForbidden", err)
	}
}
