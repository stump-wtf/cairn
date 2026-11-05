package cliclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCommentPostsArtifactAnchorAndDecodesRow(t *testing.T) {
	var body commentPayload
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/artifacts/art42/comments" {
			t.Errorf("path = %q, want the comments route", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          7,
			"anchor_type": "artifact",
			"actor_id":    "sam@stump.rocks",
			"actor_kind":  "human",
			"body":        body.Body,
		})
	}))
	defer srv.Close()

	comment, err := New(srv.URL, "pat_token").Comment(context.Background(), "art42", CommentOptions{Body: "looks good"})
	if err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if gotAuth != "Bearer pat_token" {
		t.Errorf("Authorization = %q, want the bearer token attached", gotAuth)
	}
	if body.AnchorType != AnchorArtifact {
		t.Errorf("anchor_type = %q, want %q — the server requires one", body.AnchorType, AnchorArtifact)
	}
	if body.ParentID != nil {
		t.Errorf("parent_id = %v, want omitted for a thread root", *body.ParentID)
	}
	if comment.ID != 7 || comment.ActorID != "sam@stump.rocks" {
		t.Fatalf("comment = %+v, want id 7 by sam@stump.rocks", comment)
	}
}

func TestCommentReplySendsParentID(t *testing.T) {
	var body commentPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":8,"anchor_type":"artifact","body":"reply"}`))
	}))
	defer srv.Close()

	parent := int64(7)
	if _, err := New(srv.URL, "t").Comment(context.Background(), "art42", CommentOptions{Body: "reply", ParentID: &parent}); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if body.ParentID == nil || *body.ParentID != 7 {
		t.Fatalf("parent_id = %v, want 7", body.ParentID)
	}
}

func TestCommentMissingScopeMapsToSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "forbidden", "message": "comment requires the annotations:write scope"},
		})
	}))
	defer srv.Close()

	_, err := New(srv.URL, "tok").Comment(context.Background(), "art42", CommentOptions{Body: "hi"})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("Comment error = %v, want ErrForbidden", err)
	}
}

func TestListCommentsGetsThreadAndToleratesEmptyBody(t *testing.T) {
	var gotPath, gotAuth string
	responses := map[string]string{
		"art42": `{"comments":[{"id":2,"anchor_type":"artifact","parent_id":1,"actor_id":"sam@stump.rocks",
			"actor_kind":"human","body":"agreed","created_at":"2026-11-05T11:00:00Z"},
			{"id":3,"anchor_type":"artifact","actor_id":"bot@stump.rocks","actor_kind":"agent",
			"body":"","deleted":true,"created_at":"2026-11-05T12:00:00Z"}]}`,
		"quiet": `{}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET: listing comments is a read", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/artifacts/"), "/comments")
		_, _ = io.WriteString(w, responses[id])
	}))
	defer srv.Close()

	comments, err := New(srv.URL, "pat_token").ListComments(context.Background(), "art42")
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if gotPath != "/v1/artifacts/art42/comments" {
		t.Errorf("path = %q, want the comments route", gotPath)
	}
	if gotAuth != "Bearer pat_token" {
		t.Errorf("Authorization = %q, want the bearer token attached when one is resolved", gotAuth)
	}
	if len(comments) != 2 {
		t.Fatalf("comments = %d, want the 2 the server listed", len(comments))
	}
	if comments[0].ParentID == nil || *comments[0].ParentID != 1 {
		t.Errorf("reply parent_id = %v, want 1", comments[0].ParentID)
	}
	if !comments[1].Deleted {
		t.Error("tombstone lost its deleted flag")
	}

	quiet, err := New(srv.URL, "").ListComments(context.Background(), "quiet")
	if err != nil {
		t.Fatalf("ListComments with no token: %v", err)
	}
	if quiet == nil || len(quiet) != 0 {
		t.Fatalf("comments = %#v, want an empty slice a caller can range over", quiet)
	}
}

func TestListCommentsUnknownArtifactMapsToSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "not_found", "message": "artifact not found"},
		})
	}))
	defer srv.Close()

	_, err := New(srv.URL, "").ListComments(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ListComments error = %v, want ErrNotFound", err)
	}
}
