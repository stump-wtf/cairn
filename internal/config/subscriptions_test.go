package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Governing: SPEC-0023 REQ "Removing the Instance-Wide Outbound Targets",
// REQ "Owned Outbound Subscriptions", REQ "Subscription Target Safety"

// removedVars are the two instance-wide outbound variables, spelled in two
// halves so this file is not itself a match for the search it performs.
var removedVars = []string{
	"CAIRN_OUTBOUND_" + "WEBHOOK_URLS",
	"CAIRN_OUTBOUND_" + "WEBHOOK_SECRET",
}

// TestNoCodeReadsRemovedOutboundVariables is scenario "No code reads the
// variables": no Go source file in the module names either variable.
func TestNoCodeReadsRemovedOutboundVariables(t *testing.T) {
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found from the test directory: %v", err)
	}
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "website", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		for _, v := range removedVars {
			if strings.Contains(string(b), v) {
				t.Errorf("%s names the removed variable %s", path, v)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// A positive control: the walk must have seen the module's Go files, or
	// an empty result would prove nothing.
	if scanned < 50 {
		t.Fatalf("scanned only %d Go files; the walk did not reach the source tree", scanned)
	}
	if b, err := os.ReadFile("config.go"); err != nil || !strings.Contains(string(b), "CAIRN_ENCRYPTION_KEY") {
		t.Fatal("positive control failed: config.go should name CAIRN_ENCRYPTION_KEY")
	}
}

// TestLeftoverOutboundVariablesAreIgnored is the configuration half of "A
// leftover variable delivers nothing": a deployment upgraded with the old
// variables still set loads normally, and nothing in the loaded
// configuration carries their values.
func TestLeftoverOutboundVariablesAreIgnored(t *testing.T) {
	const target = "https://switchboard.example/webhooks/w/leftover-token"
	const secret = "leftover-signing-secret-leftover-signing"
	t.Setenv(removedVars[0], target)
	t.Setenv(removedVars[1], secret)
	t.Setenv("CAIRN_BASE_URL", "https://cairn.example.com")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() with the removed variables set = %v, want a normal start", err)
	}
	if got := strings.Join([]string{c.EncryptionKeyRaw, c.APITokensRaw, c.OperatorsRaw}, " "); strings.Contains(got, "leftover") {
		t.Fatal("a removed variable's value reached the configuration")
	}
}

func TestSubscriptionSettings(t *testing.T) {
	t.Setenv("CAIRN_BASE_URL", "https://cairn.example.com")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.OutboundAllowHTTP {
		t.Fatal("CAIRN_OUTBOUND_ALLOW_HTTP defaults to on; it must be off")
	}
	if c.SubscriptionsPerUser != 5 || c.SubscriptionsPerTeam != 10 {
		t.Fatalf("ceilings = %d / %d, want 5 / 10", c.SubscriptionsPerUser, c.SubscriptionsPerTeam)
	}

	t.Setenv("CAIRN_OUTBOUND_ALLOW_HTTP", "true")
	t.Setenv("CAIRN_SUBSCRIPTIONS_PER_USER", "2")
	t.Setenv("CAIRN_SUBSCRIPTIONS_PER_TEAM", "3")
	if c, err = Load(); err != nil {
		t.Fatal(err)
	}
	if !c.OutboundAllowHTTP || c.SubscriptionsPerUser != 2 || c.SubscriptionsPerTeam != 3 {
		t.Fatalf("got allow_http=%v ceilings=%d/%d", c.OutboundAllowHTTP, c.SubscriptionsPerUser, c.SubscriptionsPerTeam)
	}

	for _, bad := range []string{"0", "-1", "1001", "lots"} {
		t.Setenv("CAIRN_SUBSCRIPTIONS_PER_USER", bad)
		if _, err := Load(); err == nil {
			t.Fatalf("CAIRN_SUBSCRIPTIONS_PER_USER=%q loaded; want an error", bad)
		}
	}
}
