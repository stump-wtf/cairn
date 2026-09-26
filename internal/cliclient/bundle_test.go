package cliclient

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

func TestOpenBundleFilesDeduplicatesSamePath(t *testing.T) {
	dir := t.TempDir()
	a := writeTempFile(t, dir, "a.log", "hello")

	files, closeAll, err := OpenBundleFiles([]string{a, a})
	if err != nil {
		t.Fatalf("OpenBundleFiles: %v", err)
	}
	defer closeAll()

	if len(files) != 1 {
		t.Fatalf("OpenBundleFiles returned %d files, want 1 (deduplicated)", len(files))
	}
}

func TestOpenBundleFilesDeduplicatesRelativeAndAbsolute(t *testing.T) {
	dir := t.TempDir()
	abs := writeTempFile(t, dir, "a.log", "hello")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	defer func() { _ = os.Chdir(wd) }()

	files, closeAll, err := OpenBundleFiles([]string{"a.log", abs})
	if err != nil {
		t.Fatalf("OpenBundleFiles: %v", err)
	}
	defer closeAll()

	if len(files) != 1 {
		t.Fatalf("OpenBundleFiles returned %d files, want 1 (relative == absolute)", len(files))
	}
}

// symlinkedDir returns a symlink pointing at dir, skipping the test if the
// platform will not create one (unprivileged Windows).
func symlinkedDir(t *testing.T, dir string) string {
	t.Helper()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	return link
}

// TestOpenBundleFilesDeduplicatesThroughSymlinkedDir is the portable form of
// the bug behind cairn#31: de-duplication used to compare absolute path
// *strings*, so one file reached through two spellings was uploaded twice.
//
// The symlink is constructed explicitly rather than inherited from the
// platform's temp-directory layout. macOS gets one for free — /var is a
// symlink to /private/var, so t.TempDir() plus a Chdir was already enough to
// trip this — but Linux does not, which is exactly why CI stayed green while
// the bug was live on every developer's Mac. Building the symlink here means
// a regression fails everywhere.
//
// Governing: SPEC-0008 REQ "Oversize and Duplicate File Handling"
func TestOpenBundleFilesDeduplicatesThroughSymlinkedDir(t *testing.T) {
	dir := t.TempDir()
	direct := writeTempFile(t, dir, "a.log", "hello")
	viaLink := filepath.Join(symlinkedDir(t, dir), "a.log")

	if direct == viaLink {
		t.Fatal("test is not exercising two spellings of one file")
	}

	files, closeAll, err := OpenBundleFiles([]string{direct, viaLink})
	if err != nil {
		t.Fatalf("OpenBundleFiles: %v", err)
	}
	defer closeAll()

	if len(files) != 1 {
		t.Fatalf("OpenBundleFiles returned %d files, want 1 — %q and %q are one file",
			len(files), direct, viaLink)
	}
}

// TestDedupPathsDeduplicatesThroughSymlinkedDir covers the other half of
// cairn#31. dedupPaths is what `cairn add` actually runs (via
// ValidateBundlePaths and PrepareBundleFilesConcurrent); OpenBundleFiles is a
// separate code path, and both carried the same string comparison.
//
// Governing: SPEC-0008 REQ "Oversize and Duplicate File Handling"
func TestDedupPathsDeduplicatesThroughSymlinkedDir(t *testing.T) {
	dir := t.TempDir()
	direct := writeTempFile(t, dir, "a.log", "hello")
	viaLink := filepath.Join(symlinkedDir(t, dir), "a.log")

	got, err := dedupPaths([]string{direct, viaLink})
	if err != nil {
		t.Fatalf("dedupPaths: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("dedupPaths returned %d paths, want 1 — %q and %q are one file",
			len(got), direct, viaLink)
	}
	// First-seen order and the caller's own spelling are part of the
	// contract: the survivor is the argument the user typed first.
	if got[0] != direct {
		t.Errorf("dedupPaths kept %q, want the first-seen spelling %q", got[0], direct)
	}
}

// TestDedupPathsDeduplicatesHardLinks pins the other way one file wears two
// names. Nothing about a hard link is resolvable by canonicalising strings —
// neither name is "the real one" — so this is the case that would survive any
// path-normalisation fix and only falls to comparing device and inode.
//
// Governing: SPEC-0008 REQ "Oversize and Duplicate File Handling"
func TestDedupPathsDeduplicatesHardLinks(t *testing.T) {
	dir := t.TempDir()
	original := writeTempFile(t, dir, "a.log", "hello")
	link := filepath.Join(dir, "b.log")
	if err := os.Link(original, link); err != nil {
		t.Skipf("hard links unsupported here: %v", err)
	}

	got, err := dedupPaths([]string{original, link})
	if err != nil {
		t.Fatalf("dedupPaths: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("dedupPaths returned %d paths, want 1 — %q is a hard link to %q",
			len(got), link, original)
	}
}

// TestDedupPathsKeepsDistinctFiles is the guard against over-matching: an
// identity comparison that returned true too readily would silently drop
// files the user asked to upload, which is a worse failure than the duplicate
// it replaced. Same size, same contents, same directory — different inodes.
func TestDedupPathsKeepsDistinctFiles(t *testing.T) {
	dir := t.TempDir()
	a := writeTempFile(t, dir, "a.log", "identical")
	b := writeTempFile(t, dir, "b.log", "identical")

	got, err := dedupPaths([]string{a, b})
	if err != nil {
		t.Fatalf("dedupPaths: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("dedupPaths returned %d paths, want 2 — %q and %q are different files",
			len(got), a, b)
	}
}

// TestOpenBundleFilesMissingPathIsStillOpenError pins the error a missing
// path produces. De-duplication now stats first, and a stat failure must not
// become the reported error: os.Open runs immediately after and already says
// the right thing. Duplicate missing paths fall back to string matching, so
// they collapse to one error rather than two.
func TestOpenBundleFilesMissingPathIsStillOpenError(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.log")

	_, _, err := OpenBundleFiles([]string{missing, missing})
	if err == nil {
		t.Fatal("OpenBundleFiles: want error for missing path")
	}
	if !strings.HasPrefix(err.Error(), "open ") {
		t.Errorf("OpenBundleFiles error = %q, want the open error, not the stat probe", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("OpenBundleFiles error = %q, want it to wrap fs.ErrNotExist", err)
	}
}

func TestOpenBundleFilesMissingPathIsError(t *testing.T) {
	dir := t.TempDir()
	_, _, err := OpenBundleFiles([]string{filepath.Join(dir, "nope.log")})
	if err == nil {
		t.Fatal("OpenBundleFiles: want error for missing path")
	}
}

func TestOpenBundleFilesRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	_, _, err := OpenBundleFiles([]string{dir})
	if err == nil {
		t.Fatal("OpenBundleFiles: want error for a directory path")
	}
}

func TestCreateBundleSendsAllFilesInOneRequest(t *testing.T) {
	dir := t.TempDir()
	a := writeTempFile(t, dir, "a.log", "log contents")
	b := writeTempFile(t, dir, "b.sql", "select 1;")

	var gotParts []string
	var gotTitle string
	var gotTTL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTTL = r.Header.Get("X-Cairn-Ttl-Seconds")
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "multipart/form-data" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			if part.FileName() != "" {
				gotParts = append(gotParts, part.FileName())
			} else if part.FormName() == "title" {
				buf := make([]byte, 64)
				n, _ := part.Read(buf)
				gotTitle = string(buf[:n])
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Artifact{ID: "bundle1", URL: "https://cairn.sh/bundle1", Size: 21})
	}))
	defer srv.Close()

	files, closeAll, err := OpenBundleFiles([]string{a, b})
	if err != nil {
		t.Fatalf("OpenBundleFiles: %v", err)
	}
	defer closeAll()

	c := New(srv.URL, "tok")
	art, err := c.CreateBundle(context.Background(), files, CreateBundleOptions{Title: "my bundle", TTLSeconds: 86400})
	if err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}
	if art.ID != "bundle1" {
		t.Errorf("art.ID = %q", art.ID)
	}
	if len(gotParts) != 2 {
		t.Fatalf("server saw %d file parts, want 2: %v", len(gotParts), gotParts)
	}
	if gotTitle != "my bundle" {
		t.Errorf("title field = %q", gotTitle)
	}
	if gotTTL != "86400" {
		t.Errorf("X-Cairn-Ttl-Seconds = %q, want 86400", gotTTL)
	}
}

// TestCreateBundleSendsTagsHeader: bundle tags travel as the same
// comma-separated X-Cairn-Tags header on the one multipart request.
//
// Governing: ADR-0018, SPEC-0008 REQ "Bundle Creation"
func TestCreateBundleSendsTagsHeader(t *testing.T) {
	dir := t.TempDir()
	a := writeTempFile(t, dir, "a.log", "log contents")

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Cairn-Tags")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Artifact{ID: "bundle1", URL: "https://cairn.sh/bundle1"})
	}))
	defer srv.Close()

	files, closeAll, err := OpenBundleFiles([]string{a})
	if err != nil {
		t.Fatalf("OpenBundleFiles: %v", err)
	}
	defer closeAll()

	c := New(srv.URL, "tok")
	if _, err := c.CreateBundle(context.Background(), files, CreateBundleOptions{
		Tags: []string{"handoff", "size:s"},
	}); err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}
	if want := "handoff,size:s"; got != want {
		t.Errorf("X-Cairn-Tags = %q, want %q", got, want)
	}
}
