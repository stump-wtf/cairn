package cliclient

import (
	"context"
	"encoding/json"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestOpenBundleFilesDeduplicatesRelativeAndAbsolute only exercises symlink
// resolution on platforms whose temp dir already sits behind one (macOS:
// /var → /private/var). This pins the same contract everywhere by naming one
// file through both a real directory and a symlink to that directory.
func TestOpenBundleFilesDeduplicatesAcrossSymlinkedDir(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	a := writeTempFile(t, realDir, "a.log", "hello")
	if err := os.Symlink(realDir, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	files, closeAll, err := OpenBundleFiles([]string{a, filepath.Join(dir, "link", "a.log")})
	if err != nil {
		t.Fatalf("OpenBundleFiles: %v", err)
	}
	defer closeAll()

	if len(files) != 1 {
		t.Fatalf("OpenBundleFiles returned %d files, want 1 (symlinked dir == real dir)", len(files))
	}
}

// De-duplication canonicalizes the directory only: a symlink and its target
// named separately are two distinct members, because each carries its own
// multipart filename and silently dropping one the user named explicitly
// would surprise more than the saved upload is worth.
func TestOpenBundleFilesKeepsDistinctlyNamedSymlink(t *testing.T) {
	dir := t.TempDir()
	target := writeTempFile(t, dir, "2026-07-27.log", "hello")
	if err := os.Symlink(target, filepath.Join(dir, "latest.log")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	files, closeAll, err := OpenBundleFiles([]string{target, filepath.Join(dir, "latest.log")})
	if err != nil {
		t.Fatalf("OpenBundleFiles: %v", err)
	}
	defer closeAll()

	if len(files) != 2 {
		t.Fatalf("OpenBundleFiles returned %d files, want 2 (differently-named symlink kept)", len(files))
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
