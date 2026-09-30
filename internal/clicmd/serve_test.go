package clicmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// Governing: ADR-0031 (One Cairn Binary — The Server Is `cairn serve`).

// The one binary keeps every command it had, and gains exactly one: `serve`.
// A command that disappears here would disappear for every user on the next
// release, so the tree is pinned by name.
func TestRootCommandTreeAfterOneBinary(t *testing.T) {
	var buf bytes.Buffer
	root := NewRootCmd(IOStreams{In: &buf, Out: &buf, ErrOut: &buf}, "")

	got := map[string]bool{}
	for _, c := range root.Commands() {
		got[c.Name()] = true
	}

	for _, name := range []string{"add", "login", "logout", "whoami", "serve"} {
		if !got[name] {
			t.Errorf("root command tree lost %q after the one-binary fold (ADR-0031)", name)
		}
	}
	if len(got) != 5 {
		t.Errorf("root command tree has %d subcommands (%v), want exactly add, login, logout, whoami, serve", len(got), got)
	}
}

// `cairn serve --help` must describe the environment contract: the server is
// configured through CAIRN_* variables and takes no flags of its own
// (ADR-0031 "Config and env compatibility"). A flag creeping in here would
// mean a second way to configure the server that the deployment story does
// not know about.
func TestServeCommandEnvOnlyContract(t *testing.T) {
	var buf bytes.Buffer
	root := NewRootCmd(IOStreams{In: &buf, Out: &buf, ErrOut: &buf}, "")

	serveCmd, _, err := root.Find([]string{"serve"})
	if err != nil || serveCmd.Name() != "serve" {
		t.Fatalf("root.Find(serve) = %v, %v; want the serve command", serveCmd, err)
	}
	allowed := map[string]bool{"help": true, "version": true}
	serveCmd.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if !allowed[f.Name] {
			t.Errorf("serve carries --%s; the ADR-0031 contract is env-only configuration", f.Name)
		}
	})
	// cobra adds --help/--version lazily, at execution time; ask for them the
	// way Execute would, so the pin below checks the real surface.
	serveCmd.InitDefaultHelpFlag()
	serveCmd.InitDefaultVersionFlag()
	for _, flag := range []string{"help", "version"} {
		if serveCmd.Flags().Lookup(flag) == nil {
			t.Errorf("serve is missing the standard --%s flag", flag)
		}
	}
	if !strings.Contains(serveCmd.Long, "CAIRN_") {
		t.Errorf("serve --help does not name the CAIRN_* environment contract")
	}
}
