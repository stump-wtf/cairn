package redact

// Harness Corpus Parity
//
// SPEC-0017 RD-3: every credential Harness's internal/redact masks is also
// detected by Cairn's config, and masked the same way (label kept, value
// replaced by [REDACTED]). The cases are Harness's own fixtures from
// internal/redact/redact_test.go and lines_test.go on its main branch.
//
// Written out literally, these fixtures are exactly what this repository's
// gitleaks CI stage exists to reject, and it cannot tell a fake from the real
// thing, so they are assembled at run time from split literals and no source
// line holds one whole. Harness learned this in its PR #345.
//
// Governing: ADR-0023, SPEC-0017 RD-3
//
// @joestump 09/23/2026 - Added for cairn#289.

import (
	"context"
	"strings"
	"testing"
)

// fixtures mirrors Harness's replacer, placeholder for placeholder.
var fixtures = strings.NewReplacer(
	"<PASSWORD>", "hunter2"+"-hunter2",
	"<HEX>", strings.Repeat("0123456789abcdef", 2)+"01234567",
	"<GHS>", "gh"+"s_"+strings.Repeat("abcdefghij", 3)+"0123",
	"<GHP>", "gh"+"p_"+strings.Repeat("abcdefghij", 3)+"0123",
	"<HVS>", "hv"+"s."+"CAESI"+strings.Repeat("abcdefghij", 3),
	"<TOKEN>", "abc123"+"def456",
	"<JWT>", "abc."+"def."+"ghi",
	"<PEM-BEGIN>", "-----BEGIN OPENSSH "+"PRIVATE KEY-----",
	"<PEM-END>", "-----END OPENSSH "+"PRIVATE KEY-----",
	"<PEM-BODY>", strings.Repeat("QUJD", 16),
	"<MASK>", Mask,
)

// harnessCorpus is Harness's TestStringMasksCredentials table, plus the key
// block from its lines_test.go.
var harnessCorpus = []struct{ name, in, want string }{
	{"git remote with password",
		"git remote set-url origin https://joestump-agent:<HEX>@gitea.stump.rocks/stump.wtf/harness.git",
		"git remote set-url origin https://joestump-agent:<MASK>@gitea.stump.rocks/stump.wtf/harness.git"},
	{"x-access-token userinfo",
		"git clone https://x-access-token:<GHS>@gitea.stump.rocks/stump.wtf/harness.git",
		"git clone https://x-access-token:<MASK>@gitea.stump.rocks/stump.wtf/harness.git"},
	{"bare token userinfo",
		"git push https://<HEX>@gitea.stump.rocks/a/b.git main",
		"git push https://<MASK>@gitea.stump.rocks/a/b.git main"},
	{"authorization token header",
		`curl -s -H "Authorization: token <HEX>" https://gitea.stump.rocks/api/v1/user`,
		`curl -s -H "Authorization: token <MASK>" https://gitea.stump.rocks/api/v1/user`},
	{"authorization bearer header",
		`curl -H 'Authorization: Bearer <JWT>' https://api.example.com`,
		`curl -H 'Authorization: Bearer <MASK>' https://api.example.com`},
	{"api key header",
		`curl -H "X-API-Key: <TOKEN>" https://api.example.com`,
		`curl -H "X-API-Key: <MASK>" https://api.example.com`},
	{"env assignment",
		"GITEA_TOKEN=<TOKEN> tea pr list",
		"GITEA_TOKEN=<MASK> tea pr list"},
	{"exported quoted key",
		`export OPENAI_API_KEY="<TOKEN>"`,
		`export OPENAI_API_KEY=<MASK>`},
	{"json password",
		`{"username": "joe", "password": "<PASSWORD>"}`,
		`{"username": "joe", "password": <MASK>}`},
	{"github token",
		"echo <GHP> | gh auth login --with-token",
		"echo <MASK> | gh auth login --with-token"},
	{"openbao token",
		"bao login <HVS>",
		"bao login <MASK>"},
	{"password flag",
		"mysql --password <PASSWORD> -h db",
		"mysql --password <MASK> -h db"},
	{"token flag with equals",
		"tea login add --token=<TOKEN> --url https://gitea.stump.rocks",
		"tea login add --token=<MASK> --url https://gitea.stump.rocks"},
	{"curl basic auth",
		"curl -fsS -u admin:<PASSWORD> https://prowlarr.stump.rocks/api",
		"curl -fsS -u admin:<MASK> https://prowlarr.stump.rocks/api"},
	{"private key",
		"cat > id <<EOF\n<PEM-BEGIN>\nb3BlbnNzaC1rZXktdjEAAAAA\n<PEM-END>\nEOF",
		"cat > id <<EOF\n<MASK>\nEOF"},
	{"private key, lines_test body",
		"$ cat id_ed25519\n<PEM-BEGIN>\n<PEM-BODY>\n<PEM-BODY>\n<PEM-END>\n$ ls",
		"$ cat id_ed25519\n<MASK>\n$ ls"},
}

// TestHarnessCorpusParity: each Harness fixture is detected, and the masked
// text is exactly what Harness produces.
func TestHarnessCorpusParity(t *testing.T) {
	s := newTestScanner(t)
	for _, tc := range harnessCorpus {
		t.Run(tc.name, func(t *testing.T) {
			in, want := fixtures.Replace(tc.in), fixtures.Replace(tc.want)
			got, o, err := s.Text(context.Background(), "body", in, ModeMask)
			if err != nil {
				t.Fatalf("Text: %v", err)
			}
			if o.Status != StatusMasked || o.Count < 1 {
				t.Fatalf("outcome = %+v, want masked with at least one finding", o)
			}
			if got != want {
				t.Errorf("masked\n got %q\nwant %q", got, want)
			}
			if again, o2, err := s.Text(context.Background(), "body", got, ModeMask); err != nil || again != got || o2.Status != StatusClean {
				t.Errorf("masking is not idempotent: %q -> %q (%+v, %v)", got, again, o2, err)
			}
		})
	}
}

// TestHarnessCorpusRejects: the same fixtures, in reject mode, are refused
// with the rule named and without the value in the message.
func TestHarnessCorpusRejects(t *testing.T) {
	s := newTestScanner(t)
	for _, tc := range harnessCorpus {
		t.Run(tc.name, func(t *testing.T) {
			in := fixtures.Replace(tc.in)
			got, o, err := s.Text(context.Background(), "body", in, ModeReject)
			if err == nil {
				t.Fatalf("Text accepted %q in reject mode", in)
			}
			if got != "" {
				t.Errorf("rejection returned text %q", got)
			}
			assertNoSecretIn(t, err.Error(), in, tc.in)
			if len(o.Findings) == 0 || !strings.Contains(err.Error(), "rule "+o.Findings[0].Rule) {
				t.Errorf("error %q does not name the rule of %+v", err, o.Findings)
			}
		})
	}
}

// TestHarnessNegatives: Harness's "leave ordinary commands alone" corpus.
// Cairn runs gitleaks' default rules as well, so this pins which of those
// lines Cairn leaves unchanged; any that a default rule flags is listed with
// the reason instead of silently tolerated.
func TestHarnessNegatives(t *testing.T) {
	s := newTestScanner(t)
	for _, in := range []string{
		"git log --oneline 8799476587bf1234567890abcdef1234567890ab",
		"/home/joestump-agent/src/stumpcloud/infra/pdx.yaml",
		"ssh -o ConnectTimeout=15 -o BatchMode=yes joestump@nuc01.stump.wtf 'uptime; echo ---; sudo docker ps'",
		"git clone git@github.com:stump-wtf/harness.git",
		"curl -s 'https://api.example.com/v1/chat?max_tokens=4096'",
		"https://mastodon.social/@joestump",
		`token := os.Getenv("GITEA_TOKEN")`,
		"docker run --rm -u 1000:1000 ghcr.io/stump-wtf/harness:latest",
		"go test ./internal/runtrace -run TestAttribute -count=1",
		"mcp_gitea_search_issues",
		"Bad Request: litellm.ContextWindowExceededError: maximum context length is 196608 tokens",
		"task-abcdefghijklmnopqrstuvwxyz0123456789",
		"TOKEN=$(cat /tmp/gitea-token 2>/dev/null) || TOKEN=$(grep -oE 'https://[^:]+:[^@]+@gitea.stump.rocks' ~/.git-credentials)",
		`if [ -z "${TOKEN:-}" ]; then echo NO_TOKEN; fi`,
		`export GITEA_TOKEN="$(cat /tmp/gitea-token)"`,
		`curl -sS -H "Authorization: token $TOKEN" https://gitea.stump.rocks/api/v1/user`,
		"git push https://x-access-token:$GH_TOKEN@gitea.stump.rocks/stump.wtf/harness.git",
	} {
		got, o, err := s.Text(context.Background(), "body", in, ModeMask)
		if err != nil || got != in || o.Status != StatusClean {
			t.Errorf("Text(%q) = %q, %+v, %v; want it unchanged and clean", in, got, o, err)
		}
	}
}

// assertNoSecretIn fails if s contains any credential-bearing piece of the
// fixture: every placeholder's expansion that appears in the raw input.
func assertNoSecretIn(t *testing.T, s, in, template string) {
	t.Helper()
	for _, ph := range []string{"<PASSWORD>", "<HEX>", "<GHS>", "<GHP>", "<HVS>", "<TOKEN>", "<JWT>", "<PEM-BODY>"} {
		if !strings.Contains(template, ph) {
			continue
		}
		v := fixtures.Replace(ph)
		if strings.Contains(s, v) {
			t.Errorf("%q leaks the fixture value for %s", s, ph)
		}
	}
	if strings.Contains(s, in) {
		t.Errorf("%q contains the whole input line", s)
	}
}
