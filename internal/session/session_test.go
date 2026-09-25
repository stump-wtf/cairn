package session

import (
	"context"
	"testing"
	"time"
)

// TestMemoryStoreLifecycle exercises the create → get → delete path and proves
// the raw token is returned only at creation while the record retains no plaintext
// token (hash-at-rest).
func TestMemoryStoreLifecycle(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()

	sess, err := st.Create(ctx, "", "", "joe", "", time.Hour)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.Token == "" || sess.CSRFToken == "" {
		t.Fatal("create must return raw token and csrf token")
	}
	if sess.ActorID != "joe" {
		t.Fatalf("actor = %q, want joe", sess.ActorID)
	}

	// The raw token is never persisted in the clear: the map is keyed by hash and
	// the stored record carries no token value.
	if rec, ok := st.byHash[hashToken(sess.Token)]; !ok {
		t.Fatal("session not stored under its hash")
	} else if rec.Token != "" {
		t.Fatal("stored record must not retain the raw token")
	}

	got, err := st.Get(ctx, sess.Token)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ActorID != "joe" || got.CSRFToken != sess.CSRFToken {
		t.Fatalf("get returned wrong session: %+v", got)
	}

	if err := st.Delete(ctx, sess.Token); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.Get(ctx, sess.Token); err != ErrNotFound {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
}

// TestMemoryStoreExpiry proves an expired session resolves as absent, matching
// the Postgres store's expires_at filter.
func TestMemoryStoreExpiry(t *testing.T) {
	st := NewMemoryStore()
	base := time.Now()
	st.now = func() time.Time { return base }
	ctx := context.Background()

	sess, err := st.Create(ctx, "", "", "joe", "", time.Minute)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Advance past expiry.
	st.now = func() time.Time { return base.Add(2 * time.Minute) }
	if _, err := st.Get(ctx, sess.Token); err != ErrNotFound {
		t.Fatalf("expired get = %v, want ErrNotFound", err)
	}
}

// TestMemoryStoreUnknownAndEmpty proves unknown and empty tokens are uniformly
// absent, and that create rejects a blank actor.
func TestMemoryStoreUnknownAndEmpty(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()

	if _, err := st.Get(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("unknown get = %v, want ErrNotFound", err)
	}
	if _, err := st.Get(ctx, ""); err != ErrNotFound {
		t.Fatalf("empty get = %v, want ErrNotFound", err)
	}
	if err := st.Delete(ctx, ""); err != nil {
		t.Fatalf("delete empty = %v, want nil (idempotent)", err)
	}
	if _, err := st.Create(ctx, "", "", "", "", time.Hour); err == nil {
		t.Fatal("create with empty actor must fail")
	}
}

// TestTokensAreDistinct proves minted tokens are unique per call (high entropy),
// so two sessions never collide.
func TestTokensAreDistinct(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		s, err := st.Create(ctx, "", "", "joe", "", time.Hour)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if seen[s.Token] {
			t.Fatal("duplicate session token minted")
		}
		seen[s.Token] = true
	}
}
