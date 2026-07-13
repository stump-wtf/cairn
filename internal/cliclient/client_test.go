package cliclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeErrorSuccessConsumesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if err := DecodeError(resp); err != nil {
		t.Fatalf("DecodeError on 2xx: %v", err)
	}
}

func TestDecodeErrorParsesADR0012Envelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":       "not_found",
				"message":    "artifact 9qz1a does not exist or has expired",
				"details":    map[string]string{"id": "9qz1a"},
				"request_id": "req_7Kx2p",
			},
		})
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	err = DecodeError(resp)
	if err == nil {
		t.Fatal("DecodeError: want error for 404")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("DecodeError: want *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != CodeNotFound {
		t.Errorf("Code = %q, want %q", apiErr.Code, CodeNotFound)
	}
	if apiErr.Message != "artifact 9qz1a does not exist or has expired" {
		t.Errorf("Message = %q", apiErr.Message)
	}
	if apiErr.RequestID != "req_7Kx2p" {
		t.Errorf("RequestID = %q", apiErr.RequestID)
	}
	if apiErr.Details["id"] != "9qz1a" {
		t.Errorf("Details[id] = %q", apiErr.Details["id"])
	}
	if apiErr.HTTPStatus != http.StatusNotFound {
		t.Errorf("HTTPStatus = %d", apiErr.HTTPStatus)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Error("errors.Is(err, ErrNotFound) = false, want true")
	}
	if errors.Is(err, ErrForbidden) {
		t.Error("errors.Is(err, ErrForbidden) = true, want false")
	}
}

func TestDecodeErrorNonEnvelopeBodyBecomesInternal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	err = DecodeError(resp)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("DecodeError: want *APIError, got %T", err)
	}
	if apiErr.Code != CodeInternal {
		t.Errorf("Code = %q, want %q", apiErr.Code, CodeInternal)
	}
	if apiErr.HTTPStatus != http.StatusBadGateway {
		t.Errorf("HTTPStatus = %d", apiErr.HTTPStatus)
	}
}

func TestAPIErrorIsMatchesByCodeOnly(t *testing.T) {
	decoded := &APIError{Code: CodeUnauthorized, Message: "token expired", RequestID: "req_abc"}
	if !errors.Is(decoded, ErrNotAuthenticated) {
		t.Error("a decoded unauthorized error should match ErrNotAuthenticated regardless of message/request_id")
	}
	if errors.Is(decoded, ErrForbidden) {
		t.Error("an unauthorized error should not match ErrForbidden")
	}
}

func TestClientDoAttachesAuthAndUserAgent(t *testing.T) {
	var gotAuth, gotUA, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "secret-token")
	resp, err := c.Do(context.Background(), http.MethodGet, "/v1/workspaces/me", nil, "")
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization header = %q", gotAuth)
	}
	if gotUA != "cairn-cli" {
		t.Errorf("User-Agent header = %q", gotUA)
	}
	if gotPath != "/v1/workspaces/me" {
		t.Errorf("path = %q", gotPath)
	}
}

func TestClientDoTransportFailureWrapsErrNetwork(t *testing.T) {
	c := New("http://127.0.0.1:1", "") // port 0/1 refuses connections immediately
	_, err := c.Do(context.Background(), http.MethodGet, "/v1/bin", nil, "")
	if err == nil {
		t.Fatal("Do: want error for unreachable host")
	}
	if !errors.Is(err, ErrNetwork) {
		t.Errorf("Do error = %v, want wrapping ErrNetwork", err)
	}
}

func TestCreateArtifactSuccess(t *testing.T) {
	var gotBody, gotTitle, gotSha, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		gotTitle = r.Header.Get("X-Cairn-Title")
		gotSha = r.Header.Get("X-Cairn-Sha256")
		gotAuth = r.Header.Get("Authorization")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Artifact{
			ID:   "9qz1a",
			URL:  "https://cairn.sh/9qz1a",
			Size: 5,
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	art, err := c.CreateArtifact(context.Background(), strings.NewReader("hello"), CreateArtifactOptions{
		Title:  "notes.md",
		SHA256: "deadbeef",
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	if art.ID != "9qz1a" || art.URL != "https://cairn.sh/9qz1a" {
		t.Errorf("art = %+v", art)
	}
	if gotBody != "hello" {
		t.Errorf("server saw body %q", gotBody)
	}
	if gotTitle != "notes.md" {
		t.Errorf("X-Cairn-Title = %q", gotTitle)
	}
	if gotSha != "deadbeef" {
		t.Errorf("X-Cairn-Sha256 = %q", gotSha)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

func TestCreateArtifactServerRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "payload_too_large", "message": "body exceeds the configured limit"},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	_, err := c.CreateArtifact(context.Background(), strings.NewReader("huge"), CreateArtifactOptions{})
	if err == nil {
		t.Fatal("CreateArtifact: want error")
	}
	if !errors.Is(err, ErrOversize) {
		t.Errorf("CreateArtifact error = %v, want wrapping ErrOversize", err)
	}
}
