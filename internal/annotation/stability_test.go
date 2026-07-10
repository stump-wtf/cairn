package annotation

import (
	"strings"
	"testing"
)

func TestBlockIDDeterministic(t *testing.T) {
	a := BlockID(3, "## Deploy steps")
	b := BlockID(3, "## Deploy steps")
	if a != b {
		t.Fatalf("same position+content must yield the same block id: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "b_") || len(a) != 2+blockIDHexLen {
		t.Fatalf("block id %q must be b_ + %d hex chars", a, blockIDHexLen)
	}
}

func TestBlockIDSensitivity(t *testing.T) {
	base := BlockID(3, "## Deploy steps")
	if BlockID(4, "## Deploy steps") == base {
		t.Error("a different structural position must yield a different block id")
	}
	if BlockID(3, "## Rollback steps") == base {
		t.Error("different content must yield a different block id")
	}
	if BlockID(3, "## Deploy steps  \n") != base {
		t.Error("trailing whitespace must not change a block id")
	}
	if BlockID(3, "  ## Deploy steps") == base {
		t.Error("leading whitespace is significant and must change the block id")
	}
}

func TestLineHashDeterministic(t *testing.T) {
	a := LineHash("\tif err != nil {")
	if a != LineHash("\tif err != nil {") {
		t.Fatal("same line must yield the same hash")
	}
	if len(a) != lineHashHexLen {
		t.Fatalf("line hash %q must be %d hex chars", a, lineHashHexLen)
	}
	if LineHash("\tif err != nil {\n") != a {
		t.Error("the line ending must not change the hash")
	}
	if LineHash("if err != nil {") == a {
		t.Error("indentation is code: leading whitespace must change the hash")
	}
	if LineHash("\tif err == nil {") == a {
		t.Error("different text must yield a different hash")
	}
}

func TestSelectionQuote(t *testing.T) {
	body := "plan → build → deploy"
	q, ok := SelectionQuote(body, 7, 12)
	if !ok || q != "build" {
		t.Fatalf("SelectionQuote = %q, %v; want \"build\", true (rune offsets)", q, ok)
	}
	if _, ok := SelectionQuote(body, 7, 7); ok {
		t.Error("empty range must not resolve")
	}
	if _, ok := SelectionQuote(body, -1, 3); ok {
		t.Error("negative start must not resolve")
	}
	if _, ok := SelectionQuote(body, 0, 1000); ok {
		t.Error("end past the body must not resolve")
	}
}

func TestQuoteMatches(t *testing.T) {
	body := "plan → build → deploy"
	if !QuoteMatches(body, 7, 12, "build") {
		t.Error("a matching quote must verify")
	}
	// A mismatch means "context changed", never a silent mis-anchor.
	if QuoteMatches(body, 7, 12, "built") {
		t.Error("a stale quote must not verify")
	}
	if QuoteMatches(body, 0, 1000, "plan") {
		t.Error("out-of-range offsets must not verify")
	}
}
