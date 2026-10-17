package config

// Guards the self-hosting sign-in surface: every variable that decides
// whether a caller can authenticate at all must be forwarded by the compose
// file the self-hosting guide tells people to run, and must be listed in
// .env.example so a fresh install has a hint that it exists. Conversely the
// development seams must stay out of that compose file: wiring them would
// hand a public instance a bypass.
//
// The report this came from set the variables in .env exactly as the docs
// instructed and every request still returned 401, because the compose file
// never passed them to the container.
//
// @joestump-agent 10/17/2026 - Added for cairn#452.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// sign-in vars the self-host compose MUST forward (empty default is fine:
// absent means the surface fails closed).
var wiredSignInVars = []string{
	"CAIRN_OIDC_ISSUER",
	"CAIRN_OIDC_CLIENT_ID",
	"CAIRN_OIDC_CLIENT_SECRET",
	"CAIRN_OIDC_TRUST_EMAIL",
	"CAIRN_GITHUB_CLIENT_ID",
	"CAIRN_GITHUB_CLIENT_SECRET",
	"CAIRN_API_TOKENS",
	"CAIRN_OPERATORS",
	"CAIRN_OPERATOR_GROUP",
	"CAIRN_ENCRYPTION_KEY",
}

// dev seams and risky switches that are deliberately absent from the
// self-host compose file; the guide documents each one as such.
var unwiredSeamVars = []string{
	"CAIRN_DEV_LOGIN_PASSWORD",
	"CAIRN_DEV_INSECURE_BEARER_AUTH",
	"CAIRN_OUTBOUND_ALLOW_HTTP",
}

// guideComposeVars returns the cairn service's environment map from the
// compose file published in the self-hosting guide.
func guideComposeVars(t *testing.T) map[string]string {
	t.Helper()
	src := readFile(t, "../../website/docs/guides/self-hosting.md")
	vars := map[string]string{}
	inCairn := false
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "  cairn:") {
			inCairn = true
			continue
		}
		if inCairn {
			if regexp.MustCompile(`^(  )?[A-Za-z]`).MatchString(line) {
				break
			}
			if m := regexp.MustCompile(`^\s+([A-Z][A-Z0-9_]*):\s*(.*)$`).FindStringSubmatch(line); m != nil {
				vars[m[1]] = strings.TrimSpace(m[2])
			}
		}
	}
	if len(vars) == 0 {
		t.Fatal("no cairn environment block found in the self-hosting guide")
	}
	return vars
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSelfHostComposeForwardsSignInVars(t *testing.T) {
	vars := guideComposeVars(t)
	for _, name := range wiredSignInVars {
		value, ok := vars[name]
		if !ok {
			t.Errorf("the guide's compose file never passes %s to cairnd, so setting it in .env has no effect", name)
			continue
		}
		if !strings.HasPrefix(value, "${") {
			t.Errorf("%s = %q: the compose file must expand from .env, never carry a value of its own", name, value)
		}
	}
}

func TestSelfHostComposeLeavesOutDevSeams(t *testing.T) {
	vars := guideComposeVars(t)
	for _, name := range unwiredSeamVars {
		if _, ok := vars[name]; ok {
			t.Errorf("%s is wired through the self-host compose file; it is a development seam and must stay out of the documented deploy path", name)
		}
	}
}

func TestEnvExampleListsSignInVars(t *testing.T) {
	src := readFile(t, "../../.env.example")
	for _, name := range wiredSignInVars {
		if !regexp.MustCompile(`(?m)^#?` + name + `=`).MatchString(src) {
			t.Errorf(".env.example does not mention %s", name)
		}
	}
	// A seam may appear in .env.example only commented out, so copying the
	// file cannot hand a self-hoster a bypass switch that is already on.
	for _, name := range unwiredSeamVars {
		if regexp.MustCompile(`(?m)^` + name + `=`).MatchString(src) {
			t.Errorf(".env.example sets %s active, a development seam that belongs to nobody's production file", name)
		}
	}
}
