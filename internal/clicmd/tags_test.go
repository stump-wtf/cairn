package clicmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/cliclient"
	"github.com/stump-wtf/cairn/internal/cliexit"
)

// Governing: ADR-0018 (Client-Asserted Artifact Tags), SPEC-0008 REQ "Pipe and
// Path Ingest", REQ "Bundle Creation", ADR-0025, SPEC-0019 VE-9

func TestParseTagFlags(t *testing.T) {
	tests := []struct {
		name    string
		raw     []string
		want    []string
		wantErr bool
		// wantWarn is the exact stderr: one VE-9 warning per tag the CLI
		// changed, and none for a tag it sends as given.
		wantWarn string
	}{
		{"none", nil, nil, false, ""},
		{"repeated", []string{"handoff", "lane:auto"}, []string{"handoff", "lane:auto"}, false, ""},
		{"comma list", []string{"handoff, lane:s", "issue:stump.wtf/cairn#42"}, []string{"handoff", "lane:s", "issue:stump.wtf/cairn#42"}, false, ""},
		{"ascii case folded", []string{"Handoff,size:M"}, []string{"handoff", "size:m"}, false,
			"cairn: warning: tag \"Handoff\" sent as \"handoff\"\ncairn: warning: tag \"size:M\" sent as \"size:m\"\n"},
		{"only ascii folded", []string{"Ärger:X", "lane m"}, []string{"Ärger:x", "lane m"}, false,
			"cairn: warning: tag \"Ärger:X\" sent as \"Ärger:x\"\n"},
		{"non-ascii uppercase unchanged", []string{"Ärger"}, []string{"Ärger"}, false, ""},
		{"empty flag", []string{""}, nil, true, ""},
		{"empty item", []string{"a,,b"}, nil, true, ""},
		{"trailing comma", []string{"handoff,"}, nil, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var warn strings.Builder
			got, err := parseTagFlags(tc.raw, &warn)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseTagFlags(%q) = %q, want an error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("parseTagFlags(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			if warn.String() != tc.wantWarn {
				t.Errorf("parseTagFlags(%q) warned %q, want %q", tc.raw, warn.String(), tc.wantWarn)
			}
		})
	}
}

func TestBareIngestSendsTagFlags(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Cairn-Tags")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(cliclient.Artifact{ID: "abc12", URL: "https://cairn.sh/abc12"})
	}))
	defer srv.Close()

	_, stderr, code := runCLI(t, emptyConfigPath(t), "# handoff prompt", "--url", srv.URL, "--token", "tok",
		"--tag", "handoff", "--tag", "lane:auto,issue:stump.wtf/cairn#42")
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if want := "handoff,lane:auto,issue:stump.wtf/cairn#42"; got != want {
		t.Errorf("X-Cairn-Tags = %q, want %q", got, want)
	}
}

func TestAddSendsTagFlags(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(a, []byte("# task"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Cairn-Tags")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(cliclient.Artifact{ID: "bundle1", URL: "https://cairn.sh/bundle1"})
	}))
	defer srv.Close()

	_, stderr, code := runCLI(t, emptyConfigPath(t), "", "add", a, "--url", srv.URL, "--token", "tok",
		"--tag", "handoff", "--tag", "size:m")
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if want := "handoff,size:m"; got != want {
		t.Errorf("X-Cairn-Tags = %q, want %q", got, want)
	}
}

// TestEmptyTagFlagIsUsageError: an empty --tag never reaches the network, on
// either create command.
func TestEmptyTagFlagIsUsageError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server contacted despite an empty --tag: %s %s", r.Method, r.URL)
	}))
	defer srv.Close()

	dir := t.TempDir()
	a := filepath.Join(dir, "a.md")
	if err := os.WriteFile(a, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	for _, args := range [][]string{
		{"--tag", ""},
		{"--tag", "handoff,,lane:s"},
		{"add", a, "--tag", ","},
	} {
		args = append(args, "--url", srv.URL, "--token", "tok")
		if _, _, code := runCLI(t, emptyConfigPath(t), "body", args...); code != int(cliexit.Usage) {
			t.Errorf("cairn %q exit code = %d, want usage (%d)", args, code, cliexit.Usage)
		}
	}
}
