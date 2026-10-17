package clicmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Governing: SPEC-0024 REQ "Declaring Relations on Create", REQ "Trace
// Context on Create" (CLI surface).

func TestParseRelationFlags(t *testing.T) {
	cases := []struct {
		name                      string
		replyTo, derived, follows []string
		want                      []string
		wantErr                   bool
	}{
		{
			name:    "one of each, grouped by kind",
			replyTo: []string{"a1b2c3"},
			derived: []string{"d4e5f6"},
			follows: []string{"g7h8i9"},
			want:    []string{"reply_to=a1b2c3", "derived_from=d4e5f6", "follows=g7h8i9"},
		},
		{
			name:    "repeatable and comma-separated, like --tag",
			replyTo: []string{"a1", "b2,c3"},
			want:    []string{"reply_to=a1", "reply_to=b2", "reply_to=c3"},
		},
		{
			name:    "empty id is a usage error",
			replyTo: []string{"a1,"},
			wantErr: true,
		},
		{
			name: "nothing declared",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRelationFlags(tc.replyTo, tc.derived, tc.follows)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want usage error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// captureCreate stands up a server that records the create request's relation
// and traceparent headers and answers with a minimal create response.
func captureCreate(t *testing.T) (*httptest.Server, *[]string, *string) {
	t.Helper()
	var rels []string
	var trace string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/artifacts" {
			http.NotFound(w, r)
			return
		}
		rels = r.Header.Values("X-Cairn-Relation")
		trace = r.Header.Get("traceparent")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"abc12","url":"https://cairn.sh/abc12","size":5}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &rels, &trace
}

func TestIngestSendsRelationHeadersAndTraceparent(t *testing.T) {
	t.Setenv("TRACEPARENT", "")
	srv, rels, trace := captureCreate(t)

	stdout, stderr, code := runCLI(t, emptyConfigPath(t), "hello",
		"--url", srv.URL, "--token", "tok",
		"--reply-to", "a1b2c3", "--reply-to", "d4e5f6", "--follows", "g7h8")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if strings.TrimSpace(stdout) != "https://cairn.sh/abc12" {
		t.Errorf("stdout = %q", stdout)
	}
	want := []string{"reply_to=a1b2c3", "reply_to=d4e5f6", "follows=g7h8"}
	if strings.Join(*rels, ",") != strings.Join(want, ",") {
		t.Errorf("X-Cairn-Relation = %v, want %v", *rels, want)
	}
	if *trace != "" {
		t.Errorf("traceparent sent = %q, want no header without the flag or env", *trace)
	}
}

func TestIngestTraceparentEnvDefaultAndFlagOverride(t *testing.T) {
	t.Setenv("TRACEPARENT", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	srv, _, trace := captureCreate(t)

	if _, stderr, code := runCLI(t, emptyConfigPath(t), "hello",
		"--url", srv.URL, "--token", "tok"); code != 0 {
		t.Fatalf("env-default run: exit %d, stderr=%q", code, stderr)
	}
	if *trace == "" {
		t.Fatal("TRACEPARENT env did not reach the request")
	}

	if _, stderr, code := runCLI(t, emptyConfigPath(t), "hello",
		"--url", srv.URL, "--token", "tok",
		"--traceparent", "00-11111111111111111111111111111111-2222222222222222-01"); code != 0 {
		t.Fatalf("flag run: exit %d, stderr=%q", code, stderr)
	}
	if !strings.HasPrefix(*trace, "00-11111111") {
		t.Errorf("traceparent = %q, want the flag value to win over the env default", *trace)
	}
}

func TestIngestEmptyRelationIDIsUsageErrorBeforeNetwork(t *testing.T) {
	srv, rels, _ := captureCreate(t)
	_, _, code := runCLI(t, emptyConfigPath(t), "hello",
		"--url", srv.URL, "--token", "tok", "--derived-from", "")
	if code == 0 {
		t.Fatal("empty relation id must be a usage error")
	}
	if len(*rels) != 0 {
		t.Error("request reached the server despite the usage error")
	}
}
