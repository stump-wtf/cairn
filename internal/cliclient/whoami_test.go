package cliclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWhoamiSuccessDecodesIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/whoami" {
			t.Errorf("path = %q, want /v1/whoami", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk_live_x" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Whoami{ActorID: "sam@stump.rocks", Channel: "via API", Authenticated: true})
	}))
	defer srv.Close()

	c := New(srv.URL, "sk_live_x")
	who, err := c.Whoami(context.Background())
	if err != nil {
		t.Fatalf("Whoami: %v", err)
	}
	if who.ActorID != "sam@stump.rocks" {
		t.Errorf("ActorID = %q", who.ActorID)
	}
	if who.Channel != "via API" {
		t.Errorf("Channel = %q", who.Channel)
	}
	if !who.Authenticated {
		t.Error("Authenticated = false, want true")
	}
}

func TestWhoamiUnauthorizedMapsToSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "unauthorized", "message": "authentication required"},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "bogus")
	_, err := c.Whoami(context.Background())
	if err == nil {
		t.Fatal("Whoami: want error for 401")
	}
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("Whoami error = %v, want ErrNotAuthenticated", err)
	}
}
