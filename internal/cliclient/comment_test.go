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
