package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/store"
)

// Ownership is a user id on every surface (ADR-0029, SPEC-0023 REQ "Owner
// Model"): a string-keyed credential resolves to a user, and two credentials
// naming the same actor share that user's Bin while a second user sees none
// of it.

// ownedIDs lists the ids a bearer's Bin returns.
func ownedIDs(t *testing.T, srv *httptest.Server, token string) map[string]bool {
	t.Helper()
	resp := do(t, http.MethodGet, srv.URL+"/v1/bin", token, nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/bin as %s = %d, want 200", token, resp.StatusCode)
	}
	var page binResponse
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode bin: %v", err)
	}
	ids := map[string]bool{}
	for _, a := range page.Artifacts {
		ids[a.ID] = true
	}
	return ids
}

// A CAIRN_API_TOKENS entry acts as the user its actor names: the same user a
// dev bearer with that name resolves to, so it keeps that Bin; an entry
// naming anyone else owns nothing of it, cannot delete it, and cannot change
// its policy. Against string ownership the second token's refusal would hold
// but the shared Bin would not survive the move to user ids.
func TestIntegrationStaticTokenActsAsItsUser(t *testing.T) {
	cfg := noRateLimit()
	cfg.APITokens = []APIToken{
		{Secret: "sk_live_joe_owner", ActorID: "joe@example.com"},
		{Secret: "sk_live_someone_else", ActorID: "pat@example.com"},
	}
	srv := testServer(t, cfg, store.Options{MaxUploadBytes: 1 << 20})

	id := seedArtifact(t, srv, "joe@example.com") // dev bearer, same actor
	if !ownedIDs(t, srv, "sk_live_joe_owner")[id] {
		t.Fatal("the static token naming joe@example.com does not see joe's artifact")
	}
	if ownedIDs(t, srv, "sk_live_someone_else")[id] {
		t.Fatal("a second user's Bin lists joe's artifact")
	}

	resp := do(t, http.MethodGet, srv.URL+"/v1/whoami", "sk_live_joe_owner", nil, "")
	var who whoamiResponse
	if err := json.NewDecoder(resp.Body).Decode(&who); err != nil {
		t.Fatalf("decode whoami: %v", err)
	}
	resp.Body.Close()
	if who.ActorID != "joe@example.com" {
		t.Errorf("static token renders as %q, want joe@example.com", who.ActorID)
	}

	del := do(t, http.MethodDelete, srv.URL+"/v1/artifacts/"+id, "sk_live_someone_else", nil, "")
	del.Body.Close()
	if del.StatusCode != http.StatusNotFound {
		t.Fatalf("another user's delete = %d, want 404", del.StatusCode)
	}
	vis := do(t, http.MethodPatch, srv.URL+"/v1/artifacts/"+id+"/policy", "sk_live_someone_else",
		strings.NewReader(`{"visibility":"private"}`), "application/json")
	vis.Body.Close()
	if vis.StatusCode != http.StatusForbidden {
		t.Fatalf("another user's visibility change = %d, want 403", vis.StatusCode)
	}

	del = do(t, http.MethodDelete, srv.URL+"/v1/artifacts/"+id, "sk_live_joe_owner", nil, "")
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("owner's delete through the static token = %d, want 204", del.StatusCode)
	}
}

// SPEC-0023 "A run follows its artifact": a run's owner is its artifact's
// owning user, so only that user appends to or closes it, and it is in that
// user's Bin alone.
func TestIntegrationRunFollowsItsArtifact(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20})
	resp := do(t, http.MethodPost, srv.URL+"/v1/runs", "alice",
		strings.NewReader(`{"title":"owned run","mode":"open","started_at":"2026-09-01T00:00:00Z"}`), "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create run = %d, want 201", resp.StatusCode)
	}
	var run struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	resp.Body.Close()

	if !ownedIDs(t, srv, "alice")[run.ID] || ownedIDs(t, srv, "mallory")[run.ID] {
		t.Fatal("the run is not in exactly its owner's Bin")
	}
	for _, path := range []string{"/spans", "/close"} {
		body := `{"spans":[{"span_id":"s1","category":"reason","started_at":"2026-09-01T00:00:01Z","ended_at":"2026-09-01T00:00:02Z"}]}`
		r := do(t, http.MethodPost, srv.URL+"/v1/runs/"+run.ID+path, "mallory", strings.NewReader(body), "application/json")
		r.Body.Close()
		if r.StatusCode != http.StatusForbidden {
			t.Errorf("non-owner %s = %d, want 403", path, r.StatusCode)
		}
	}
	r := do(t, http.MethodPost, srv.URL+"/v1/runs/"+run.ID+"/close", "alice", nil, "")
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("owner close = %d, want 200", r.StatusCode)
	}
}
