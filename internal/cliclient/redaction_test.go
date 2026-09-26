package cliclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Governing: ADR-0023, SPEC-0017 RD-5, RD-9, RD-10

// maskedResponse is a create response for a body the server masked, written
// out by hand in the server's wire shape.
const maskedResponse = `{"id":"abc12","url":"https://cairn.sh/abc12","size":40,
	"redaction_status":"masked","redacted":true,
	"redactions":{"count":2,"rules":{"github-pat":2}}}`

// redactionServer records the X-Cairn-Redaction headers each create carried
// and answers with body.
func redactionServer(t *testing.T, body string, got *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Header.Values(RedactionHeader)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCreateSendsRedactionHeader: a set Redaction travels as one
// X-Cairn-Redaction header on both creates, and an unset one sends none, so
// the server applies its own default mode (SPEC-0017 RD-5).
func TestCreateSendsRedactionHeader(t *testing.T) {
	ctx := context.Background()
	creates := map[string]func(c *Client, redaction string) error{
		"artifact": func(c *Client, redaction string) error {
			_, err := c.CreateArtifact(ctx, strings.NewReader("hi"), CreateArtifactOptions{Redaction: redaction})
			return err
		},
		"bundle": func(c *Client, redaction string) error {
			files := []BundleFile{{Path: "a.txt", Body: strings.NewReader("a"), Size: 1}}
			_, err := c.CreateBundle(ctx, files, CreateBundleOptions{Redaction: redaction})
			return err
		},
	}
	for name, create := range creates {
		t.Run(name, func(t *testing.T) {
			var got []string
			c := New(redactionServer(t, maskedResponse, &got).URL, "tok")

			if err := create(c, RedactionMask); err != nil {
				t.Fatalf("create: %v", err)
			}
			if len(got) != 1 || got[0] != "mask" {
				t.Errorf("X-Cairn-Redaction = %q, want one header \"mask\"", got)
			}

			if err := create(c, ""); err != nil {
				t.Fatalf("create: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("X-Cairn-Redaction = %q with no downgrade, want no header", got)
			}
		})
	}
}

// TestCreateDecodesRedactionOutcome: the scan outcome decodes, and survives
// the re-encode `--json` prints (SPEC-0017 RD-9, RD-10).
func TestCreateDecodesRedactionOutcome(t *testing.T) {
	var got []string
	c := New(redactionServer(t, maskedResponse, &got).URL, "tok")
	art, err := c.CreateArtifact(context.Background(), strings.NewReader("hi"), CreateArtifactOptions{})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	if !art.WasRedacted() || art.RedactionStatus != "masked" {
		t.Errorf("redacted = %v, status = %q, want true and masked", art.Redacted, art.RedactionStatus)
	}
	if art.Redactions == nil || art.Redactions.Count != 2 || art.Redactions.Rules["github-pat"] != 2 {
		t.Errorf("redactions = %+v, want count 2 and github-pat ×2", art.Redactions)
	}

	raw, err := json.Marshal(art)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{`"redacted":true`, `"redactions":{"count":2,"rules":{"github-pat":2}}`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("re-encoded artifact %s lacks %s", raw, want)
		}
	}
}

// TestWasRedacted: only an explicit redacted: true counts. A clean create and
// a server that predates scanning both read as not redacted.
func TestWasRedacted(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name string
		art  *Artifact
		want bool
	}{
		{"masked", &Artifact{Redacted: &yes}, true},
		{"clean", &Artifact{Redacted: &no}, false},
		{"old server", &Artifact{}, false},
		{"nil", nil, false},
	} {
		if got := tc.art.WasRedacted(); got != tc.want {
			t.Errorf("%s: WasRedacted = %v, want %v", tc.name, got, tc.want)
		}
	}
}
