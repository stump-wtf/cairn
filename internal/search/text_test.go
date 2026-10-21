package search

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stump-wtf/cairn/internal/artifact"
)

func TestIsTextualMedia(t *testing.T) {
	cases := []struct {
		mediaType string
		want      bool
	}{
		{"text/plain", true},
		{"text/markdown", true},
		{"text/csv;charset=utf-8", true},
		{"TEXT/HTML", true},
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"application/ld+json", true},
		{"application/rss+xml", true},
		{"application/yaml", true},
		{"application/x-sh", true},
		{"application/octet-stream", false},
		{"application/pdf", false},
		{"image/png", false},
		{"", false},
		{"not a media type!", false},
	}
	for _, c := range cases {
		if got := isTextualMedia(c.mediaType); got != c.want {
			t.Errorf("isTextualMedia(%q) = %v, want %v", c.mediaType, got, c.want)
		}
	}
}

func TestIndexesBodySkipList(t *testing.T) {
	// REQ-2: captured requests and span payloads never contribute text,
	// whatever media type they claim.
	for _, st := range []artifact.ShareType{artifact.TypeWebhook, artifact.TypeTrajectory} {
		if indexesBody(st, "text/plain") {
			t.Errorf("indexesBody(%q, text/plain) = true, want false", st)
		}
	}
	for _, st := range []artifact.ShareType{artifact.ShareType("markdown"), artifact.TypeFile, artifact.TypeBundle} {
		if !indexesBody(st, "text/markdown") {
			t.Errorf("indexesBody(%q, text/markdown) = false, want true", st)
		}
	}
	if indexesBody(artifact.TypeFile, "image/png") {
		t.Error("indexesBody(file, image/png) = true, want false")
	}
	if indexesBody(artifact.TypeFile, "application/octet-stream") {
		t.Error("indexesBody(file, octet-stream) = true, want false")
	}
	if !indexesBody(artifact.TypeFile, "application/json") {
		t.Error("indexesBody(file, application/json) = false, want true")
	}
}

func TestMemberIsText(t *testing.T) {
	if memberIsText("image/svg+xml") {
		t.Error("memberIsText(image/svg+xml) = true, want false: images are skipped by type")
	}
	if !memberIsText("application/json") {
		t.Error("memberIsText(application/json) = false, want true")
	}
	if memberIsText("application/zip") {
		t.Error("memberIsText(application/zip) = false, want true")
	}
}

func TestCappedTextKeepsUTF8Valid(t *testing.T) {
	if got := cappedText("hello", 0); got != "hello" {
		t.Errorf("cappedText with no limit = %q, want it unchanged", got)
	}
	if got := cappedText("hello", 100); got != "hello" {
		t.Errorf("cappedText under the limit = %q, want it unchanged", got)
	}

	// The cap lands inside a multi-byte rune: the rune must be dropped whole
	// rather than emitted as a broken sequence, which would make the whole
	// to_tsvector call fail.
	s := "café " + strings.Repeat("x", 10) // é is 2 bytes
	got := cappedText(s, 5)
	if !utf8.ValidString(got) {
		t.Fatalf("cappedText(%q, 5) = %q is not valid UTF-8", s, got)
	}
	if len(got) > 5 || !strings.HasPrefix(s, got) {
		t.Fatalf("cappedText(%q, 5) = %q, want a valid prefix of at most 5 bytes", s, got)
	}
	if got := cappedText(s, 4); got != "caf" {
		t.Errorf("cappedText(%q, 4) = %q, want %q", s, got, "caf")
	}

	// A 4-byte rune straddling the cap.
	s = "a🙂b"
	for limit := 1; limit <= len(s); limit++ {
		if got := cappedText(s, limit); !utf8.ValidString(got) || len(got) > limit {
			t.Errorf("cappedText(%q, %d) = %q, want a valid prefix within the cap", s, limit, got)
		}
	}
}

func TestConfigWithDefaults(t *testing.T) {
	got := Config{}.withDefaults()
	if got.Workers < 1 || got.Batch < 1 || got.PollInterval <= 0 || got.MaxAttempts < 1 {
		t.Errorf("withDefaults() = %+v, want a usable worker pool", got)
	}
	if got.Language == "" || got.Backoff <= 0 || got.BackoffMax < got.Backoff || got.MaxBodyBytes <= 0 {
		t.Errorf("withDefaults() = %+v, want language and backoff defaults", got)
	}

	// Explicit values survive.
	in := Config{Workers: 7, Batch: 3, Language: "simple", MaxBodyBytes: 128}
	out := in.withDefaults()
	if out.Workers != 7 || out.Batch != 3 || out.Language != "simple" || out.MaxBodyBytes != 128 {
		t.Errorf("withDefaults() = %+v, want the explicit values kept", out)
	}
}

func TestBackoffIsBoundedAndMonotonic(t *testing.T) {
	x := New(nil, nil, Config{Backoff: time.Second, BackoffMax: 8 * time.Second})
	prev := time.Duration(0)
	for attempts := 1; attempts <= 12; attempts++ {
		d := x.backoff(attempts)
		if d > x.cfg.BackoffMax {
			t.Fatalf("backoff(%d) = %s, want at most %s", attempts, d, x.cfg.BackoffMax)
		}
		if d < prev {
			t.Fatalf("backoff(%d) = %s, want it never to shrink (previous %s)", attempts, d, prev)
		}
		prev = d
	}
}
