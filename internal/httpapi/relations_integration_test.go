package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/store"
)

// Governing: SPEC-0024 REQ "Declaring Relations on Create", REQ "Relation Targets
// Must Be Readable by the Creator", REQ "Relations on Read", REQ "Trace Context
// on Create"; ADR-0030.

// postRelations creates with X-Cairn-Relation header values, one per relation.
func postRelations(t *testing.T, target, actor string, body io.Reader, contentType string,
	header []string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+actor)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for _, h := range header {
		req.Header.Add(relationHeader, h)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

// getRead reads /v1/artifacts/{id} as actor and decodes the read response.
func getRead(t *testing.T, srvURL, id, actor string) artifactReadResponse {
	t.Helper()
	resp := do(t, http.MethodGet, srvURL+"/v1/artifacts/"+id, actor, nil, "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("read %s = %d, want 200: %s", id, resp.StatusCode, b)
	}
	defer resp.Body.Close()
	var got artifactReadResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode read: %v", err)
	}
	return got
}

// rawJSON returns a response body as a map, after checking the status.
func rawJSON(t *testing.T, resp *http.Response, wantStatus int) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, wantStatus, b)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return out
}

// A relation declared on create reads back on both ends: forward on the artifact
// that declared it, and as referenced_by on the target it names.
func TestIntegrationRelationsRoundTripOverREST(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20})

	target := createArtifactWithTitle(t, srv.URL, "file", "alice", "the target of the edge",
		"the target", "text/plain")
	resp := postRelations(t, srv.URL+"/v1/artifacts?type=file&title=declares", "alice",
		strings.NewReader("a body"), "application/octet-stream",
		[]string{"reply_to=" + target})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create = %d, want 201: %s", resp.StatusCode, b)
	}
	art := decodeArtifact(t, resp)

	got := getRead(t, srv.URL, art.ID, "alice")
	if len(got.Relations) != 1 {
		t.Fatalf("relations = %+v, want the one declared edge", got.Relations)
	}
	rel := got.Relations[0]
	if rel.Kind != artifact.RelationReplyTo || rel.ID != target || !rel.Available {
		t.Errorf("relation = %+v, want available reply_to to %s", rel, target)
	}
	if rel.Title == "" {
		t.Errorf("relation carries no title, so the viewer has nothing to render")
	}

	back := getRead(t, srv.URL, target, "alice")
	if len(back.Relations) != 0 {
		t.Errorf("target declares relations %+v, want none", back.Relations)
	}
	if len(back.Referenced) != 1 || back.Referenced[0].ID != art.ID ||
		back.Referenced[0].Kind != artifact.RelationReplyTo {
		t.Fatalf("referenced_by = %+v, want %s replying to it", back.Referenced, art.ID)
	}
	if back.Referenced[0].Title == "" {
		t.Errorf("referrer carries no title, so the viewer has nothing to render")
	}
	if back.ReferencedByTruncated {
		t.Error("referenced_by_truncated set for one referrer")
	}
}

// The repeatable query parameter and the multipart form field carry relations the
// same way the header does, and all three sources merge.
func TestIntegrationRelationQueryAndFormSources(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20})
	target := createArtifact(t, srv.URL, "file", "alice", "a target")
	other := createArtifact(t, srv.URL, "file", "alice", "another target")

	resp := do(t, http.MethodPost,
		srv.URL+"/v1/artifacts?type=file&title=from+query&relation=derived_from%3A"+target,
		"alice", strings.NewReader("a body"), "application/octet-stream")
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create = %d, want 201: %s", resp.StatusCode, b)
	}
	art := decodeArtifact(t, resp)
	got := getRead(t, srv.URL, art.ID, "alice")
	if len(got.Relations) != 1 || got.Relations[0].Kind != artifact.RelationDerivedFrom ||
		got.Relations[0].ID != target {
		t.Fatalf("relations = %+v, want the derived_from from the query", got.Relations)
	}

	// A single-file multipart create, with the relation as a form field and a
	// second one on the header: both arrive.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("title", "from form")
	_ = mw.WriteField("relation", "follows="+other)
	fw, err := mw.CreateFormFile("file", "note.md")
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	_, _ = fw.Write([]byte("a note"))
	if err := mw.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}
	formArt := func() artifactResponse {
		resp := postRelations(t, srv.URL+"/v1/artifacts", "alice", &buf, mw.FormDataContentType(),
			[]string{"reply_to=" + target})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("multipart create = %d, want 201: %s", resp.StatusCode, b)
		}
		var a artifactResponse
		if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
			t.Fatalf("decode create: %v", err)
		}
		return a
	}()
	got = getRead(t, srv.URL, formArt.ID, "alice")
	if len(got.Relations) != 2 {
		t.Fatalf("relations = %+v, want the form field and the header", got.Relations)
	}
	if got.Relations[0].Kind != artifact.RelationReplyTo || got.Relations[1].Kind != artifact.RelationFollows {
		// The request is read query/header first, then the body, so first-occurrence
		// order puts the header's relation ahead of the form field's.
		t.Errorf("relations = %+v, want the header edge then the form edge", got.Relations)
	}
}

// Two different unknown targets are refused the same way, with bodies that are
// byte-identical once the request id is set aside: a create cannot be used to ask
// whether some particular artifact exists.
func TestIntegrationRelationTargetErrorsAreUniform(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20})

	first := postRelations(t, srv.URL+"/v1/artifacts?type=file", "alice",
		strings.NewReader("a body"), "application/octet-stream", []string{"reply_to=zzzzzzzz"})
	second := postRelations(t, srv.URL+"/v1/artifacts?type=file", "alice",
		strings.NewReader("a body"), "application/octet-stream", []string{"reply_to=yyyyyyyy"})
	for _, resp := range []*http.Response{first, second} {
		if resp.StatusCode != http.StatusBadRequest {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 400: %s", resp.StatusCode, b)
		}
	}
	unknown := decodeError(t, first)
	unreadable := decodeError(t, second)

	for _, e := range []errorEnvelope{unknown, unreadable} {
		if e.Error.Code != errs.CodeValidation {
			t.Errorf("code = %s, want validation_failed", e.Error.Code)
		}
		if len(e.Error.Violations) != 1 {
			t.Fatalf("violations = %+v, want one", e.Error.Violations)
		}
	}
	unknown.Error.RequestID = ""
	unreadable.Error.RequestID = ""
	a, err := json.Marshal(unknown)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(unreadable)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Errorf("the two refusals differ, which answers a probe:\n%s\n%s", a, b)
	}
}

// A ninth distinct relation is refused, whichever source it arrives on.
func TestIntegrationRelationCapOverREST(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20})

	var heads []string
	for i := 0; i < artifact.MaxRelations+1; i++ {
		heads = append(heads, "follows=targe"+string(rune('a'+i))+"t")
	}
	env := decodeError(t, postRelations(t, srv.URL+"/v1/artifacts?type=file", "alice",
		strings.NewReader("a body"), "application/octet-stream", heads))
	if env.Error.Code != errs.CodeValidation {
		t.Fatalf("code = %s, want validation_failed", env.Error.Code)
	}
	var seen bool
	for _, v := range env.Error.Violations {
		if v.Reason == errs.ReasonTooMany {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("violations = %+v, want a too_many", env.Error.Violations)
	}
}

// A valid traceparent is stored and surfaced on reads; a malformed one is ignored
// rather than rejected, so a broken telemetry header never loses an upload.
func TestIntegrationTraceContextOverREST(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20})
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/artifacts?type=file&title=traced",
		strings.NewReader("a traced body"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set(traceparentHeader, "00-"+traceID+"-00f067aa0ba902b7-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create = %d, want 201: %s", resp.StatusCode, b)
	}
	traced := decodeArtifact(t, resp).ID
	if got := getRead(t, srv.URL, traced, "alice").TraceID; got != traceID {
		t.Errorf("trace_id = %q, want %s", got, traceID)
	}

	resp = do(t, http.MethodPost, srv.URL+"/v1/artifacts?type=file&title=untraced", "alice",
		strings.NewReader("no trace here"), "application/octet-stream")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	plain := decodeArtifact(t, resp).ID
	if got := getRead(t, srv.URL, plain, "alice").TraceID; got != "" {
		t.Errorf("trace_id = %q, want it absent", got)
	}

	// A malformed header must not fail the create.
	req, err = http.NewRequest(http.MethodPost, srv.URL+"/v1/artifacts?type=file&title=broken+trace",
		strings.NewReader("a body"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set(traceparentHeader, "not-a-trace-context")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("a malformed traceparent failed the create: %d: %s", resp.StatusCode, b)
	}
	var badTrace artifactResponse
	if err := json.NewDecoder(resp.Body).Decode(&badTrace); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if got := getRead(t, srv.URL, badTrace.ID, "alice").TraceID; got != "" {
		t.Errorf("trace_id = %q for a malformed header, want none", got)
	}
}

// An artifact that declared no relations and carried no traceparent reads exactly
// as it did before relations existed: neither key appears at all.
func TestIntegrationReadOmitsRelationKeysWhenEmpty(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20})
	id := createArtifact(t, srv.URL, "file", "alice", "nothing declared")

	body := rawJSON(t, do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id, "alice", nil, ""),
		http.StatusOK)
	for _, key := range []string{"relations", "referenced_by", "referenced_by_truncated", "trace_id"} {
		if _, ok := body[key]; ok {
			t.Errorf("read carries %q for an artifact with no relations", key)
		}
	}
}
