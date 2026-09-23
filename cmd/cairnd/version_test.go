package main

import "testing"

func TestWantsVersion(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"--version"}, true},
		{[]string{"-version"}, true},
		// Not a bare `version`: a future `cairnd version`-shaped subcommand
		// should be a deliberate decision, not an accident of this helper.
		{[]string{"version"}, false},
		{[]string{"--version", "extra"}, false},
		{[]string{"retention", "--version"}, false},
	}
	for _, c := range cases {
		if got := wantsVersion(c.args); got != c.want {
			t.Errorf("wantsVersion(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}

func TestVersionString(t *testing.T) {
	defer func(v, c, d string) { version, commit, date = v, c, d }(version, commit, date)

	version, commit, date = "dev", "none", "unknown"
	if got := versionString(); got != "dev" {
		t.Errorf("unstamped build: got %q, want %q", got, "dev")
	}

	version, commit, date = "0.1.1", "abc1234", "2026-09-23T00:00:00Z"
	if got, want := versionString(), "0.1.1 (abc1234, 2026-09-23T00:00:00Z)"; got != want {
		t.Errorf("release build: got %q, want %q", got, want)
	}
}
