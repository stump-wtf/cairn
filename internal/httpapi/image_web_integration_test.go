package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// onePxPNG is a minimal valid 1x1 transparent PNG — real image bytes, not a
// text stand-in, so DecidePreview's isImage sniff and the inline body route's
// Content-Type both exercise the real codepath (SPEC-0002 "Previewability
// Detection at Ingest").
var onePxPNG = mustDecodePNG("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")

func mustDecodePNG(b64 string) []byte {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		panic(err)
	}
	return b
}

// createImageArtifact posts a raw binary image body (the "web upload path",
// issue #69's "ensure an image artifact can be created (web upload path;
// binary body)") and returns its public id.
func createImageArtifact(t *testing.T, srvURL, actor, title string, body []byte) string {
	t.Helper()
	resp := do(t, http.MethodPost, srvURL+"/v1/artifacts?type=image&title="+url.QueryEscape(title), actor,
		bytes.NewReader(body), "image/png")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed image artifact: status = %d, want 201", resp.StatusCode)
	}
	art := decodeArtifact(t, resp)
	if art.ShareType != "image" {
		t.Fatalf("share_type = %q, want image", art.ShareType)
	}
	return art.ID
}

// TestIntegrationImageViewer is the story's headline acceptance (SPEC-0003 REQ
// "Image Viewer", issue #69): a pushed PNG renders through the registry
// BodyViewer capability with the pin-drop frame, an initially-empty pin
// overlay, and the react-below toolbar — never the generic-file floor.
func TestIntegrationImageViewer(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createImageArtifact(t, srv.URL, "joe", "roadmap.png", onePxPNG)

	status, html := getHTML(t, srv.URL+"/"+id)
	if status != http.StatusOK {
		t.Fatalf("GET /%s = %d, want 200", id, status)
	}

	if strings.Contains(html, "generic-card") {
		t.Error("image artifact should render the rich viewer, not the generic-file card")
	}

	for _, frag := range []string{
		`class="img-viewer"`,
		`data-img-viewer`,
		`data-artifact-id="` + id + `"`,
		`data-img-frame`,
		`tabindex="0"`,
		`src="/` + id + `/image"`,
		`alt="roadmap.png"`,
		`data-img-pins`,
		`data-img-react-cluster`,
		`data-img-react-add`,
		`image.js`, // enhancement script wired in
		`>IMG<`,    // the type badge
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("image shell for %s missing %q", id, frag)
		}
	}

	// The pin/reaction MetadataPanel fields render with no JS at all (SPEC-0003
	// Scenario "Pin overlay unavailable": the reaction total must stay visible).
	for _, frag := range []string{`<dt>pins</dt><dd>0</dd>`, `<dt>reactions</dt><dd>0</dd>`} {
		if !strings.Contains(html, frag) {
			t.Errorf("image shell for %s missing metadata panel field %q", id, frag)
		}
	}
}

// TestIntegrationImageInlineBody asserts the dedicated inline route
// (handleWebImage) serves the real bytes with the real Content-Type and an
// `inline` disposition — the one route that must NOT force a download, unlike
// every other type's `/{id}/download` (SPEC-0003 REQ "Image Viewer").
func TestIntegrationImageInlineBody(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createImageArtifact(t, srv.URL, "joe", "roadmap.png", onePxPNG)

	resp, err := http.Get(srv.URL + "/" + id + "/image")
	if err != nil {
		t.Fatalf("GET /%s/image: %v", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /%s/image = %d, want 200", id, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline;") {
		t.Errorf("Content-Disposition = %q, want inline (so <img src> renders, not downloads)", cd)
	}
	body := make([]byte, len(onePxPNG))
	n, _ := resp.Body.Read(body)
	if n != len(onePxPNG) || !bytes.Equal(body[:n], onePxPNG) {
		t.Error("inline body bytes should match the uploaded PNG exactly")
	}

	// The generic sniff-proof download route still forces an attachment for the
	// SAME artifact (both routes coexist; only /image is inline).
	dl, err := http.Get(srv.URL + "/" + id + "/download")
	if err != nil {
		t.Fatalf("GET /%s/download: %v", id, err)
	}
	dl.Body.Close()
	if ct := dl.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("/download Content-Type = %q, want application/octet-stream (sniff-proof floor unchanged)", ct)
	}
}

// TestIntegrationImageNonImageBodyHasNoInlineRoute asserts an artifact whose
// declared type never resolved to image (its body did not pass isImage) gets
// no inline route — the registry InlineViewer gate, not the generic download,
// is what decides this, so a non-image body can never stream with a
// browser-renderable Content-Type through this path.
func TestIntegrationImageNonImageBodyHasNoInlineRoute(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	// Declared type=image but a text body: DecidePreview falls back to the
	// generic file type since text/plain fails isImage.
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts?type=image", "joe", strings.NewReader("not an image"), "text/plain")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed artifact: status = %d, want 201", resp.StatusCode)
	}
	art := decodeArtifact(t, resp)
	if art.ShareType == "image" {
		t.Fatalf("a text body must not classify as image share type, got %q", art.ShareType)
	}

	status, html := getHTML(t, srv.URL+"/"+art.ID)
	if status != http.StatusOK {
		t.Fatalf("GET /%s = %d, want 200", art.ID, status)
	}
	if strings.Contains(html, "img-viewer") {
		t.Error("a non-image artifact must not render the image viewer")
	}

	imgResp, err := http.Get(srv.URL + "/" + art.ID + "/image")
	if err != nil {
		t.Fatalf("GET /%s/image: %v", art.ID, err)
	}
	imgResp.Body.Close()
	if imgResp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /%s/image = %d, want 404 (uniform not-found, no inline route for a non-image type)", art.ID, imgResp.StatusCode)
	}
}

// TestIntegrationImagePinCommentRoundTrips is the SPEC-0003 headline
// annotation scenario ("Drop a pin to comment"): posting a comment with
// pin_x/pin_y through the shell's web composer produces an image_region
// anchor carrying the normalized {x,y}, which round-trips back into the
// rendered panel as the comment-item's data-pin-x/data-pin-y attributes
// image.js reads to place the marker — and bumps both the comment count and
// the artifact's pin_count rollup (ADR-0006).
func TestIntegrationImagePinCommentRoundTrips(t *testing.T) {
	srv, client := sessionServer(t)
	id := createImageArtifact(t, srv.URL, "joe", "roadmap.png", onePxPNG)

	doLogin(t, srv, client, "reviewer", "devpass").Body.Close()
	csrf := cookieValue(t, client, srv.URL, csrfCookieName)
	resp := postForm(t, client, srv.URL+"/"+id+"/comments", url.Values{
		"body":  {"looks off here"},
		"pin_x": {"0.42"},
		"pin_y": {"0.315"},
	}, csrf)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post pin comment = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	_, html := getHTML(t, srv.URL+"/"+id)
	if !strings.Contains(html, "looks off here") {
		t.Error("pin comment body should render in the panel")
	}
	if !strings.Contains(html, `data-pin-x="0.42"`) || !strings.Contains(html, `data-pin-y="0.315"`) {
		t.Errorf("pin comment should carry its normalized {x,y} as data attributes for image.js:\n%s", html)
	}
	if !strings.Contains(html, "on a pin at 42%, 31%") {
		t.Error("pin comment should carry a human anchor-context cue in the panel")
	}
	// The pin comment bumped pin_count (image-region annotations, ADR-0006) —
	// visible with no JS via the MetadataPanel field.
	if !strings.Contains(html, `<dt>pins</dt><dd>1</dd>`) {
		t.Errorf("pin count should be 1 after one image_region comment:\n%s", html)
	}

	get := do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id, "reviewer", nil, "")
	art := decodeArtifact(t, get)
	if art.PinCount != 1 {
		t.Errorf("PinCount = %d, want 1", art.PinCount)
	}
	if art.CommentCount != 1 {
		t.Errorf("CommentCount = %d, want 1", art.CommentCount)
	}
}

// TestIntegrationImagePinRejectsOutOfRangeCoordinates asserts the
// image_region locator schema (normalized fractions in [0,1], ADR-0006) is
// still the authority — the web composer's pin_x/pin_y are parsed, not
// trusted, so an out-of-range pair is rejected server-side.
func TestIntegrationImagePinRejectsOutOfRangeCoordinates(t *testing.T) {
	srv, client := sessionServer(t)
	id := createImageArtifact(t, srv.URL, "joe", "roadmap.png", onePxPNG)

	doLogin(t, srv, client, "reviewer", "devpass").Body.Close()
	csrf := cookieValue(t, client, srv.URL, csrfCookieName)
	resp := postForm(t, client, srv.URL+"/"+id+"/comments", url.Values{
		"body":  {"nope"},
		"pin_x": {"1.5"},
		"pin_y": {"0.2"},
	}, csrf)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("out-of-range pin post = %d, want 400", resp.StatusCode)
	}
}

// TestIntegrationImageWholeArtifactReaction asserts a whole-image reaction
// (the "react-below" affordance, artifact anchor) persists with its count,
// readable through the same JSON tallies endpoint image.js hydrates from, and
// bumps the artifact's reaction_count rollup (SPEC-0003, #66/#69).
func TestIntegrationImageWholeArtifactReaction(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createImageArtifact(t, srv.URL, "joe", "roadmap.png", onePxPNG)

	body, _ := json.Marshal(reactionRequest{AnchorType: "artifact", Emoji: "🔥"})
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+id+"/reactions", "joe", bytes.NewReader(body), "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("react = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	tallies := decodeTallies(t, do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id+"/reactions", "joe", nil, ""))
	found := false
	for _, tv := range tallies.Reactions {
		if tv.AnchorType == "artifact" && tv.Emoji == "🔥" {
			found = true
			if tv.Count != 1 {
				t.Errorf("tally count = %d, want 1", tv.Count)
			}
			if !tv.Reacted {
				t.Error("the reacting actor's own tally should report reacted=true")
			}
		}
	}
	if !found {
		t.Fatal("expected an artifact-anchor 🔥 tally after reacting")
	}

	get := do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id, "joe", nil, "")
	art := decodeArtifact(t, get)
	if art.ReactionCount != 1 {
		t.Errorf("ReactionCount = %d, want 1", art.ReactionCount)
	}

	_, html := getHTML(t, srv.URL+"/"+id)
	if !strings.Contains(html, `<dt>reactions</dt><dd>1</dd>`) {
		t.Errorf("reaction count should be 1 in the metadata panel:\n%s", html)
	}
}

// TestIntegrationImageNoJSFallback asserts the SPEC-0003 Scenario "Pin
// overlay unavailable": with image.js never having run (this test never
// executes any JS — it only inspects the server-rendered HTML), the image
// still renders and the whole-artifact comment composer (pure HTMX, no
// image.js dependency) is present and posts.
func TestIntegrationImageNoJSFallback(t *testing.T) {
	srv, client := sessionServer(t)
	id := createImageArtifact(t, srv.URL, "joe", "roadmap.png", onePxPNG)

	doLogin(t, srv, client, "reviewer", "devpass").Body.Close()
	_, html := getHTMLClient(t, client, srv.URL+"/"+id)

	// The image element itself needs no script to paint.
	if !strings.Contains(html, `src="/`+id+`/image"`) {
		t.Error("the <img> should be present in the server-rendered HTML with no JS required")
	}
	// The whole-artifact comment composer is a plain HTMX form (hx-post), not
	// gated behind image.js.
	if !strings.Contains(html, `hx-post="/`+id+`/comments"`) {
		t.Error("the whole-artifact comment composer should be server-rendered and postable with no image.js")
	}

	csrf := cookieValue(t, client, srv.URL, csrfCookieName)
	resp := postForm(t, client, srv.URL+"/"+id+"/comments", url.Values{"body": {"still works without JS"}}, csrf)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("whole-artifact comment post = %d, want 200 (must work without image.js)", resp.StatusCode)
	}
	resp.Body.Close()
}
