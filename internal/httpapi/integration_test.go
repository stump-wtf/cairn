package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/joestump/cairn/internal/db"
	"github.com/joestump/cairn/internal/objectstore"
	"github.com/joestump/cairn/internal/store"
)

// schemaSeq disambiguates test schemas created within the same nanosecond.
var schemaSeq atomic.Int64

// testServer stands up the /v1 adapter over a real Postgres store (env-gated on
// CAIRN_TEST_DATABASE_URL) and an in-memory object store. Each server runs in a
// private, per-test Postgres schema — `go test ./...` runs packages in parallel
// against the one CI database, so these tests must never touch the tables the
// store and annotation package integration tests use concurrently (the same
// isolation the annotation package's newTestPool relies on).
func testServer(t *testing.T, cfg Config, opts store.Options) *httptest.Server {
	t.Helper()
	pool := newTestPool(t)
	st := store.New(pool, objectstore.NewMemory(), opts)
	srv := httptest.NewServer(New(st, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// newTestPool connects to CAIRN_TEST_DATABASE_URL (skipping otherwise) and
// applies the embedded migrations inside a private, per-test schema, dropped on
// cleanup. A fresh schema starts empty, so no TRUNCATE of shared tables — and
// therefore no interference with other packages' integration tests — is needed.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CAIRN_TEST_DATABASE_URL to run httpapi integration tests")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("httpapi_test_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))

	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), fmt.Sprintf(`DROP SCHEMA %q CASCADE`, schema)); err != nil {
			t.Errorf("drop schema: %v", err)
		}
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func do(t *testing.T, method, url, token string, body io.Reader, contentType string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, url, err)
	}
	return resp
}

func decodeArtifact(t *testing.T, resp *http.Response) artifactResponse {
	t.Helper()
	defer resp.Body.Close()
	var a artifactResponse
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		t.Fatalf("decode artifact: %v", err)
	}
	return a
}

func decodeError(t *testing.T, resp *http.Response) errorEnvelope {
	t.Helper()
	defer resp.Body.Close()
	var e errorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	return e
}

func noRateLimit() Config {
	return Config{BaseURL: "https://cairn.sh", MaxUploadBytes: 1 << 20, DefaultTTL: time.Hour}
}

func TestIntegrationCreateReadDownloadDelete(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20})
	body := "hello over the /v1 API"

	// Create (raw body).
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts?type=markdown&title=greeting", "alice",
		strings.NewReader(body), "text/markdown")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	art := decodeArtifact(t, resp)
	if art.ID == "" || len(art.ID) != 8 {
		t.Fatalf("bad public id %q", art.ID)
	}
	if art.URL != "https://cairn.sh/"+art.ID {
		t.Fatalf("url = %q, want short URL", art.URL)
	}
	if art.Checksum != sha256Hex([]byte(body)) {
		t.Fatalf("checksum = %q, want %s", art.Checksum, sha256Hex([]byte(body)))
	}
	// Provenance channel is server-derived to the REST surface, not a client claim.
	if art.Provenance.Channel != "via API" {
		t.Fatalf("channel = %q, want via API (server-derived)", art.Provenance.Channel)
	}

	// Read.
	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+art.ID, "", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Download body: re-verifiable, non-executable disposition.
	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+art.ID+"/body", "", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("body status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("body content-type = %q, want application/octet-stream", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("body disposition = %q, want attachment", cd)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing nosniff header")
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != body {
		t.Fatalf("downloaded %q, want %q", got, body)
	}
	if resp.Header.Get("X-Cairn-Checksum") != sha256Hex([]byte(body)) {
		t.Fatalf("checksum header mismatch")
	}

	// Delete (owner) then read → uniform 404.
	resp = do(t, http.MethodDelete, srv.URL+"/v1/artifacts/"+art.ID, "alice", nil, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+art.ID, "", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", env.Error.Code)
	}
}

func TestIntegrationUnauthenticatedCreateRejected(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20})
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts", "", strings.NewReader("x"), "text/plain")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "unauthorized" {
		t.Fatalf("code = %q, want unauthorized", env.Error.Code)
	}
}

func TestIntegrationUniformNotFound(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20})
	resp := do(t, http.MethodGet, srv.URL+"/v1/artifacts/zzzzzzzz", "", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", env.Error.Code)
	}
}

func TestIntegrationOversizeRejected(t *testing.T) {
	srv := testServer(t, Config{MaxUploadBytes: 8, DefaultTTL: time.Hour}, store.Options{MaxUploadBytes: 8})
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts", "alice",
		strings.NewReader("this is definitely more than eight bytes"), "text/plain")
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "payload_too_large" {
		t.Fatalf("code = %q, want payload_too_large", env.Error.Code)
	}
}

func TestIntegrationBinPagination(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20})
	const n = 5
	for i := 0; i < n; i++ {
		resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts?type=markdown", "alice",
			strings.NewReader("bin body "+string(rune('a'+i))), "text/markdown")
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("seed create %d = %d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}

	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		url := srv.URL + "/v1/bin?limit=2"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		resp := do(t, http.MethodGet, url, "alice", nil, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("bin status = %d", resp.StatusCode)
		}
		var page binResponse
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			t.Fatalf("decode bin: %v", err)
		}
		resp.Body.Close()
		for _, a := range page.Artifacts {
			if seen[a.ID] {
				t.Fatalf("bin duplicated %s across pages", a.ID)
			}
			seen[a.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > n+2 {
			t.Fatal("bin pagination did not terminate")
		}
	}
	if len(seen) != n {
		t.Fatalf("bin returned %d artifacts, want %d", len(seen), n)
	}

	// The Bin is workspace-scoped: another principal sees none of alice's items.
	resp := do(t, http.MethodGet, srv.URL+"/v1/bin", "bob", nil, "")
	var page binResponse
	json.NewDecoder(resp.Body).Decode(&page)
	resp.Body.Close()
	if len(page.Artifacts) != 0 {
		t.Fatalf("bob's Bin leaked %d of alice's artifacts", len(page.Artifacts))
	}
}

func TestIntegrationBundleCreateAndMemberRead(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("title", "my bundle")
	for _, f := range []struct{ name, body string }{{"a.txt", "AAA"}, {"b.txt", "BBBB"}} {
		fw, err := mw.CreateFormFile("file", f.name)
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		fw.Write([]byte(f.body))
	}
	mw.Close()

	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts", "alice", &buf, mw.FormDataContentType())
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("bundle create = %d, want 201", resp.StatusCode)
	}
	art := decodeArtifact(t, resp)
	if art.ShareType != "bundle" {
		t.Fatalf("share_type = %q, want bundle", art.ShareType)
	}

	// Member addressable as <id>/<name>.
	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+art.ID+"/members/a.txt", "", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member read = %d, want 200", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "AAA" {
		t.Fatalf("member bytes = %q, want AAA", got)
	}

	// Unknown member → uniform 404.
	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+art.ID+"/members/missing.txt", "", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing member = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestIntegrationRateLimited(t *testing.T) {
	// One token, negligible refill: the second request in a burst is 429.
	srv := testServer(t, Config{MaxUploadBytes: 1 << 20, DefaultTTL: time.Hour, RatePerSecond: 0.001, RateBurst: 1},
		store.Options{MaxUploadBytes: 1 << 20})

	resp := do(t, http.MethodGet, srv.URL+"/v1/artifacts/zzzzzzzz", "", nil, "")
	resp.Body.Close() // consumes the one token (404, but allowed through the limiter)

	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/zzzzzzzz", "", nil, "")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
	if env := decodeError(t, resp); env.Error.Code != "rate_limited" {
		t.Fatalf("code = %q, want rate_limited", env.Error.Code)
	}
}
