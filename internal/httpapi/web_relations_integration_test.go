package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Governing: SPEC-0024 REQ "Viewer Rendering" (SPEC-0024 slice 2: the web
// viewer). Titles arrive escaped; a target the reader cannot open renders as
// fixed copy that names nothing.

func TestIntegrationViewerRelationsHeaderAndReferrers(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())

	targetID := createArtifactWithTitle(t, srv.URL, "markdown", "joe", "the body replied to",
		"the <b>target</b> title", "text/markdown")
	// A target that stops being readable while the reply still points at it:
	// its row must render as fixed copy naming no id (SPEC-0024 REQ "Relations
	// on Read", hidden-target scenario).
	goneID := createWithTTL(t, srv.URL, "joe", "a body with a short ttl", time.Second)

	replyID := createWithRelations(t, srv.URL, "joe", "a reply",
		[]string{"reply_to=" + goneID, "derived_from=" + targetID, "follows=" + targetID})

	time.Sleep(1500 * time.Millisecond)

	status, html := getHTML(t, srv.URL+"/"+replyID)
	if status != http.StatusOK {
		t.Fatalf("GET shell = %d, want 200", status)
	}
	for _, frag := range []string{
		`data-relations`,
		`>In reply to<`,
		`an artifact you can't open`,
		`>Derived from<`,
		`&lt;b&gt;target&lt;/b&gt; title`, // escaped, and not raw
		`>Follows<`,
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("relations header missing %q", frag)
		}
	}
	if strings.Contains(html, `<b>target</b>`) {
		t.Error("target title rendered unescaped")
	}
	if strings.Contains(html, goneID) {
		t.Error("page names an id of a target the reader cannot open")
	}

	status, html = getHTML(t, srv.URL+"/"+targetID)
	if status != http.StatusOK {
		t.Fatalf("GET target shell = %d, want 200", status)
	}
	if !strings.Contains(html, "Replies and derivatives") || !strings.Contains(html, replyID) {
		t.Errorf("referrer list missing from the target's shell")
	}
	if !strings.Contains(html, "a reply") {
		t.Errorf("referrer list does not name the replying artifact")
	}
}

func TestIntegrationViewerTraceRow(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := postBodyTrace(t, srv.URL, "joe", "traced body", "text/markdown",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")

	_, html := getHTML(t, srv.URL+"/"+id)
	if !strings.Contains(html, "4bf92f3577b34da6a3ce929d0e0e4736") {
		t.Error("shell does not show the trace id a read returns")
	}
}

// createWithRelations creates a titled markdown artifact declaring the given
// relation entries (each "kind=id") and returns its public id.
func createWithRelations(t *testing.T, srvURL, actor, title string, relations []string) string {
	t.Helper()
	resp := postRelations(t, srvURL+"/v1/artifacts?type=markdown&title="+url.QueryEscape(title),
		actor, strings.NewReader("a body for "+title), "text/markdown", relations)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create as %s = %d: %s", actor, resp.StatusCode, b)
	}
	return artifactID(t, resp.Body)
}

// createWithTTL creates a markdown artifact that stops being readable after
// ttl, so a relation can be seen pointing at something that has gone away.
func createWithTTL(t *testing.T, srvURL, actor, body string, ttl time.Duration) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+"/v1/artifacts?type=markdown", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+actor)
	req.Header.Set("Content-Type", "text/markdown")
	req.Header.Set(ttlHeader, strconv.Itoa(int(ttl.Seconds())))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create as %s = %d: %s", actor, resp.StatusCode, b)
	}
	return artifactID(t, resp.Body)
}

// postBodyTrace creates with a traceparent header.
func postBodyTrace(t *testing.T, srvURL, actor, body, contentType, traceparent string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+"/v1/artifacts?type=markdown", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+actor)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(traceparentHeader, traceparent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create as %s = %d: %s", actor, resp.StatusCode, b)
	}
	return artifactID(t, resp.Body)
}

// artifactID decodes a create response's id.
func artifactID(t *testing.T, r io.Reader) string {
	t.Helper()
	var out artifactResponse
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if out.ID == "" {
		t.Fatal("create response carried no id")
	}
	return out.ID
}
