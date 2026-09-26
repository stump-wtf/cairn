package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/objectstore"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestStreamBlobComputesHashAndSize(t *testing.T) {
	obj := objectstore.NewMemory()
	body := []byte("hello cairn, this is a body")

	staged, err := streamBlob(context.Background(), obj, bytes.NewReader(body), 1<<20, "")
	if err != nil {
		t.Fatalf("streamBlob: %v", err)
	}
	if staged.sha256 != sha256Hex(body) {
		t.Fatalf("sha256 = %s, want %s", staged.sha256, sha256Hex(body))
	}
	if staged.size != int64(len(body)) {
		t.Fatalf("size = %d, want %d", staged.size, len(body))
	}
	if obj.Len() != 1 {
		t.Fatalf("expected 1 staging object, got %d", obj.Len())
	}
	// Text bodies sniff to text/plain.
	if !strings.HasPrefix(staged.mediaType, "text/plain") {
		t.Fatalf("mediaType = %q, want text/plain...", staged.mediaType)
	}
}

func TestStreamBlobDeclaredMediaWins(t *testing.T) {
	obj := objectstore.NewMemory()
	staged, err := streamBlob(context.Background(), obj, strings.NewReader("x=1"), 1<<20, "text/x-ini")
	if err != nil {
		t.Fatalf("streamBlob: %v", err)
	}
	if staged.mediaType != "text/x-ini" {
		t.Fatalf("mediaType = %q, want declared text/x-ini", staged.mediaType)
	}
}

func TestStreamBlobIdenticalBytesSameHash(t *testing.T) {
	obj := objectstore.NewMemory()
	body := []byte("dedup me")
	a, err := streamBlob(context.Background(), obj, bytes.NewReader(body), 1<<20, "")
	if err != nil {
		t.Fatalf("streamBlob a: %v", err)
	}
	b, err := streamBlob(context.Background(), obj, bytes.NewReader(body), 1<<20, "")
	if err != nil {
		t.Fatalf("streamBlob b: %v", err)
	}
	if a.sha256 != b.sha256 {
		t.Fatalf("identical bytes hashed differently: %s vs %s", a.sha256, b.sha256)
	}
	if a.stagingKey == b.stagingKey {
		t.Fatal("staging keys should be unique per upload")
	}
}

func TestStreamBlobOversizeRejectedMidStream(t *testing.T) {
	obj := objectstore.NewMemory()
	body := bytes.Repeat([]byte("A"), 1024)

	_, err := streamBlob(context.Background(), obj, bytes.NewReader(body), 512, "")
	if err == nil {
		t.Fatal("expected an oversize error")
	}
	if errs.CodeOf(err) != errs.CodePayloadTooLarge {
		t.Fatalf("code = %q, want %q", errs.CodeOf(err), errs.CodePayloadTooLarge)
	}
	if !errors.Is(err, errs.ErrTooLarge) {
		t.Fatal("error should wrap ErrTooLarge")
	}
	if obj.Len() != 0 {
		t.Fatalf("no partial object should persist, got %d", obj.Len())
	}
}

func TestStreamBlobExactLimitAccepted(t *testing.T) {
	obj := objectstore.NewMemory()
	body := bytes.Repeat([]byte("B"), 512)
	staged, err := streamBlob(context.Background(), obj, bytes.NewReader(body), 512, "")
	if err != nil {
		t.Fatalf("a body of exactly the limit should be accepted: %v", err)
	}
	if staged.size != 512 {
		t.Fatalf("size = %d, want 512", staged.size)
	}
}

func TestStreamBlobContextCancelled(t *testing.T) {
	obj := objectstore.NewMemory()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the stream begins

	_, err := streamBlob(ctx, obj, bytes.NewReader([]byte("some bytes")), 1<<20, "")
	if err == nil {
		t.Fatal("expected a context-cancelled error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error should wrap context.Canceled, got %v", err)
	}
	if obj.Len() != 0 {
		t.Fatalf("no object should persist after cancel, got %d", obj.Len())
	}
}
