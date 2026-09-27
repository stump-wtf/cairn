package httpapi

// CLI Redaction Contract Tests
//
// The CLI's redaction tests (internal/clicmd, internal/cliclient) answer with
// hand-written responses, because the CLI never imports the server
// (ADR-0003). These drive the CLI's own client against the real server with a
// scanner wired in, so a renamed outcome key, a dropped X-Cairn-Redaction
// header or a changed rejection field fails here instead of silently turning
// off the masked-content warning or the "how to proceed" line.
//
// Governing: ADR-0023, SPEC-0017 RD-5, RD-9, RD-10, RD-11; ADR-0025, SPEC-0019
// VE-8
//
// @joestump 09/27/2026 - Added in review of cairn#398.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/cliclient"
)

// cliSecretViolation returns the single secret_detected violation err
// carries, as the CLI decodes it.
func cliSecretViolation(t *testing.T, err error) cliclient.Violation {
	t.Helper()
	var ae *cliclient.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("create err = %v, want an *APIError", err)
	}
	if len(ae.Violations) != 1 || ae.Violations[0].Reason != cliclient.ReasonSecretDetected {
		t.Fatalf("violations = %+v, want one %s", ae.Violations, cliclient.ReasonSecretDetected)
	}
	return ae.Violations[0]
}

// wantCLIMasked checks the outcome the CLI's warning is built from.
func wantCLIMasked(t *testing.T, art *cliclient.Artifact, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("masked create: %v", err)
	}
	if !art.WasRedacted() || art.RedactionStatus != "masked" || art.Redactions == nil ||
		art.Redactions.Count != 1 || art.Redactions.Rules["github-pat"] != 1 {
		t.Fatalf("outcome = status %q redacted %v redactions %+v, want masked, 1 github-pat",
			art.RedactionStatus, art.Redacted, art.Redactions)
	}
}

// TestCLIRedactionAgainstServer: what `cairn main.go` and `cairn add a.txt
// c.env` send, answered by the real server. A Go file (the media type the CLI
// detects for .go) and a bundle refuse a token by default, with the field and
// line the CLI names in its RD-11 line; --redact=mask stores it masked and
// the response decodes to the count and rule the CLI's warning prints; a
// clean create decodes as not redacted, so no warning is printed.
func TestCLIRedactionAgainstServer(t *testing.T) {
	srv, _, logs := redactionServer(t)
	ctx := context.Background()
	c := cliclient.New(srv.URL, "alice")
	tok := plantedToken(40)
	code := "package x\n\nvar k = \"" + tok + "\"\n"

	single := func(redaction string) (*cliclient.Artifact, error) {
		return c.CreateArtifact(ctx, strings.NewReader(code), cliclient.CreateArtifactOptions{
			Title: "main.go", MediaType: "text/x-go", Redaction: redaction,
		})
	}
	bundle := func(redaction string) (*cliclient.Artifact, error) {
		env := "TOKEN=" + tok + "\n"
		files := []cliclient.BundleFile{
			{Path: "a.txt", Body: strings.NewReader("a"), Size: 1},
			{Path: "c.env", Body: strings.NewReader(env), Size: int64(len(env))},
		}
		return c.CreateBundle(ctx, files, cliclient.CreateBundleOptions{Redaction: redaction})
	}

	t.Run("single file refused", func(t *testing.T) {
		_, err := single("")
		v := cliSecretViolation(t, err)
		line, ok := v.Line.Int()
		if v.Field != "body" || v.Rule != "github-pat" || !ok || line != 3 || v.Value != nil {
			t.Errorf("violation = %+v (line %d), want body, github-pat, line 3, no value", v, line)
		}
	})
	t.Run("single file masked", func(t *testing.T) {
		art, err := single(cliclient.RedactionMask)
		wantCLIMasked(t, art, err)
	})
	t.Run("bundle member refused", func(t *testing.T) {
		_, err := bundle("")
		v := cliSecretViolation(t, err)
		line, ok := v.Line.Int()
		if v.Field != "members[1].content" || v.Rule != "github-pat" || !ok || line != 1 {
			t.Errorf("violation = %+v (line %d), want members[1].content, github-pat, line 1", v, line)
		}
	})
	t.Run("bundle masked", func(t *testing.T) {
		art, err := bundle(cliclient.RedactionMask)
		wantCLIMasked(t, art, err)
	})
	t.Run("clean create", func(t *testing.T) {
		art, err := c.CreateArtifact(ctx, strings.NewReader("package x\n"), cliclient.CreateArtifactOptions{MediaType: "text/x-go"})
		if err != nil {
			t.Fatal(err)
		}
		if art.WasRedacted() || art.RedactionStatus != "clean" {
			t.Errorf("clean create: status %q redacted %v, want clean and not redacted", art.RedactionStatus, art.Redacted)
		}
	})
	assertNoToken(t, "server log", logs.String(), []string{tok})
}
