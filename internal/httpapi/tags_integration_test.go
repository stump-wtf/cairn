package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/cairn/internal/outboundhook"
	"github.com/joestump/cairn/internal/store"
)

// Governing: ADR-0018 (Client-Asserted Artifact Tags), SPEC-0002 REQ "Artifact
// Tags", SPEC-0007 REQ "MCP Tool Surface — Create & Push", SPEC-0012 REQ
// "Event Payload"

// waitN blocks until at least n deliveries have arrived, returning them in
// arrival order.
func (r *hookReceiver) waitN(t *testing.T, n int) []hookedEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		if len(r.seen) >= n {
			out := append([]hookedEvent(nil), r.seen...)
			r.mu.Unlock()
			return out
		}
		got := len(r.seen)
		r.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("got %d webhook deliveries, want %d", got, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runTagEmitter starts an unsigned emitter delivering to recv for the test.
func runTagEmitter(t *testing.T, recv *hookReceiver) *outboundhook.Emitter {
	t.Helper()
	em := outboundhook.New([]string{recv.srv.URL}, "", "https://cairn.test",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); em.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return em
}

type taggedEvent struct {
	Kind string `json:"kind"`
	Data struct {
		ID         string   `json:"id"`
		ShareType  string   `json:"share_type"`
		OnBehalfOf string   `json:"on_behalf_of"`
		Tags       []string `json:"tags"`
	} `json:"data"`
}

// eventFor finds the delivery for artifact id among evs.
func eventFor(t *testing.T, evs []hookedEvent, id string) (taggedEvent, []byte) {
	t.Helper()
	for _, ev := range evs {
		var te taggedEvent
		if err := json.Unmarshal(ev.body, &te); err != nil {
			t.Fatalf("event body not JSON: %v", err)
		}
		if te.Data.ID == id {
			return te, ev.body
		}
	}
	t.Fatalf("no artifact.created delivery for %s among %d", id, len(evs))
	return taggedEvent{}, nil
}

func postTagged(t *testing.T, target, actor string, body io.Reader, contentType string, tagHeaders ...string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+actor)
	req.Header.Set("Content-Type", contentType)
	for _, h := range tagHeaders {
		req.Header.Add("X-Cairn-Tags", h)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func tagQuery(lists ...string) string {
	q := url.Values{}
	for _, l := range lists {
		q.Add("tag", l)
	}
	return q.Encode()
}

func binIDs(t *testing.T, srvURL, actor, query string) []string {
	t.Helper()
	resp := do(t, http.MethodGet, srvURL+"/v1/bin?"+query, actor, nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/bin?%s = %d", query, resp.StatusCode)
	}
	defer resp.Body.Close()
	var bin binResponse
	if err := json.NewDecoder(resp.Body).Decode(&bin); err != nil {
		t.Fatalf("decode bin: %v", err)
	}
	ids := make([]string, 0, len(bin.Artifacts))
	for _, a := range bin.Artifacts {
		ids = append(ids, a.ID)
	}
	slices.Sort(ids)
	return ids
}

func assertTagError(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "validation_failed" || env.Error.Details["field"] != "tag" {
		t.Fatalf("error = %+v, want validation_failed naming field tag", env.Error)
	}
}

// TestIntegrationTagsRESTRoundTrip: query and header tags on a raw-body create
// are merged and deduplicated, come back from create, read and the Bin, filter
// the Bin, reach the outbound event, and render in their own web panel section.
func TestIntegrationTagsRESTRoundTrip(t *testing.T) {
	recv := newHookReceiver(t)
	srv := testServer(t, noRateLimit(), store.Options{Emitter: runTagEmitter(t, recv)})

	want := []string{"handoff", "lane:m", "size:m", "issue:stump.wtf/cairn#42"}
	resp := postTagged(t,
		srv.URL+"/v1/artifacts?type=markdown&"+tagQuery("handoff,lane:m", "handoff"),
		"alice", strings.NewReader("# do the thing"), "text/markdown",
		"size:m, issue:stump.wtf/cairn#42", "lane:m")
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create = %d, want 201: %s", resp.StatusCode, b)
	}
	art := decodeArtifact(t, resp)
	if !slices.Equal(art.Tags, want) {
		t.Fatalf("create tags = %q, want merged and deduplicated %q", art.Tags, want)
	}

	got := decodeArtifact(t, do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+art.ID, "", nil, ""))
	if !slices.Equal(got.Tags, want) {
		t.Fatalf("read tags = %q, want %q", got.Tags, want)
	}

	plainID := createArtifact(t, srv.URL, "text", "alice", "plain")
	all := []string{art.ID, plainID}
	slices.Sort(all)
	if got := binIDs(t, srv.URL, "alice", ""); !slices.Equal(got, all) {
		t.Fatalf("unfiltered bin = %q, want %q", got, all)
	}
	if got := binIDs(t, srv.URL, "alice", tagQuery("handoff")); !slices.Equal(got, []string{art.ID}) {
		t.Fatalf("bin tag=handoff = %q, want only %s", got, art.ID)
	}
	if got := binIDs(t, srv.URL, "alice", tagQuery("handoff,size:l")); len(got) != 0 {
		t.Fatalf("bin tag=handoff,size:l (AND) = %q, want none", got)
	}
	assertTagError(t, do(t, http.MethodGet, srv.URL+"/v1/bin?"+tagQuery("Handoff"), "alice", nil, ""))

	evs := recv.waitN(t, 2)
	ev, raw := eventFor(t, evs, art.ID)
	if !slices.Equal(ev.Data.Tags, want) {
		t.Fatalf("event tags = %q, want %q", ev.Data.Tags, want)
	}
	if bytes.Contains(raw, []byte(`"on_behalf_of"`)) {
		t.Fatalf("REST create has no on-behalf-of, but the event carries the key: %s", raw)
	}
	if _, plainRaw := eventFor(t, evs, plainID); bytes.Contains(plainRaw, []byte(`"tags"`)) {
		t.Fatalf("untagged event carries a tags key: %s", plainRaw)
	}

	status, html := getHTML(t, srv.URL+"/"+art.ID)
	if status != http.StatusOK {
		t.Fatalf("GET /%s = %d", art.ID, status)
	}
	if !strings.Contains(html, `id="tags-h"`) || !strings.Contains(html, `<li class="tag">issue:stump.wtf/cairn#42</li>`) {
		t.Error("the web panel must render a Tags section listing each tag")
	}

	rawRead := do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+plainID, "", nil, "")
	b, _ := io.ReadAll(rawRead.Body)
	rawRead.Body.Close()
	if bytes.Contains(b, []byte(`"tags"`)) {
		t.Fatalf("untagged read carries a tags key: %s", b)
	}
	if _, html := getHTML(t, srv.URL+"/"+plainID); strings.Contains(html, `id="tags-h"`) {
		t.Error("an untagged artifact must not render a Tags section")
	}
}

// TestIntegrationTagsRESTRejectsInvalid: every bound rejects the whole create
// with validation_failed naming the tag field, and persists nothing; repeats
// are deduplicated rather than rejected.
func TestIntegrationTagsRESTRejectsInvalid(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{})

	distinct := func(from, n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf("t%d", from+i)
		}
		return strings.Join(parts, ",")
	}
	tests := []struct {
		name    string
		query   []string
		headers []string
	}{
		{"uppercase", []string{"Handoff"}, nil},
		{"space inside a tag", []string{"lane m"}, nil},
		{"equals", []string{"lane=m"}, nil},
		{"too long", []string{strings.Repeat("t", 65)}, nil},
		{"control character", []string{"a\x01b"}, nil},
		{"33 distinct across query and header", []string{distinct(0, 20)}, []string{distinct(20, 13)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertTagError(t, postTagged(t, srv.URL+"/v1/artifacts?"+tagQuery(tc.query...), "carol",
				strings.NewReader("body"), "text/plain", tc.headers...))
		})
	}
	if got := binIDs(t, srv.URL, "carol", ""); len(got) != 0 {
		t.Fatalf("rejected creates persisted %d artifacts", len(got))
	}

	repeats := strings.TrimSuffix(strings.Repeat("handoff,", 40), ",")
	resp := postTagged(t, srv.URL+"/v1/artifacts?"+tagQuery(repeats), "carol", strings.NewReader("body"), "text/plain")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("40 repeats of one tag = %d, want 201", resp.StatusCode)
	}
	if got := decodeArtifact(t, resp).Tags; !slices.Equal(got, []string{"handoff"}) {
		t.Fatalf("repeated tag stored as %q, want one handoff", got)
	}
}

// TestIntegrationTagsMultipart: `tag` form fields merge with header tags on a
// bundle, which emits artifact.created carrying them; a single-file multipart
// create takes them too; an oversized field is rejected whole.
func TestIntegrationTagsMultipart(t *testing.T) {
	recv := newHookReceiver(t)
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20, Emitter: runTagEmitter(t, recv)})

	form := func(tagFields []string, files ...string) (*bytes.Buffer, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("title", "handoff bundle")
		for _, f := range tagFields {
			_ = mw.WriteField("tag", f)
		}
		for _, name := range files {
			fw, err := mw.CreateFormFile("file", name)
			if err != nil {
				t.Fatalf("form file: %v", err)
			}
			_, _ = fw.Write([]byte("contents of " + name))
		}
		_ = mw.Close()
		return &buf, mw.FormDataContentType()
	}

	body, ct := form([]string{"handoff", "lane:auto,size:s"}, "prompt.md", "context.log")
	resp := postTagged(t, srv.URL+"/v1/artifacts", "dave", body, ct, "source:crush/run-1")
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("bundle create = %d, want 201: %s", resp.StatusCode, b)
	}
	bundle := decodeArtifact(t, resp)
	// Header tags are read before the body's form fields.
	want := []string{"source:crush/run-1", "handoff", "lane:auto", "size:s"}
	if bundle.ShareType != "bundle" || !slices.Equal(bundle.Tags, want) {
		t.Fatalf("bundle = %s tags %q, want a bundle tagged %q", bundle.ShareType, bundle.Tags, want)
	}
	ev, _ := eventFor(t, recv.waitN(t, 1), bundle.ID)
	if ev.Kind != "artifact.created" || ev.Data.ShareType != "bundle" || !slices.Equal(ev.Data.Tags, want) {
		t.Fatalf("bundle event = %+v, want a bundle artifact.created tagged %q", ev, want)
	}

	body, ct = form([]string{"handoff"}, "only.md")
	single := decodeArtifact(t, postTagged(t, srv.URL+"/v1/artifacts", "dave", body, ct))
	if single.ShareType == "bundle" || !slices.Equal(single.Tags, []string{"handoff"}) {
		t.Fatalf("single-file multipart = %s tags %q", single.ShareType, single.Tags)
	}

	body, ct = form([]string{strings.Repeat("t", maxTagFieldBytes+1)}, "a.md", "b.md")
	assertTagError(t, postTagged(t, srv.URL+"/v1/artifacts", "dave", body, ct))
}

// TestIntegrationMCPTagsHandoff: both MCP create tools publish tags and the
// handoff convention on their schema, normalize and persist the tags, return
// them on read, and emit artifact.created carrying them beside the
// server-derived on_behalf_of; a string-encoded tag array is unwrapped; a bad
// tag is a tool error.
func TestIntegrationMCPTagsHandoff(t *testing.T) {
	recv := newHookReceiver(t)
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{Emitter: runTagEmitter(t, recv)})
	token := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	sess := mcpClient(t, srv, token, nil, "claude-code")

	tools, err := sess.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, name := range []string{"artifact_create", "bundle_create"} {
		var found bool
		for _, tl := range tools.Tools {
			if tl.Name != name {
				continue
			}
			found = true
			root, _ := tl.InputSchema.(map[string]any)
			props, _ := root["properties"].(map[string]any)
			tags, _ := props["tags"].(map[string]any)
			items, _ := tags["items"].(map[string]any)
			if tags["type"] != "array" || items["type"] != "string" {
				t.Errorf("%s tags schema = %v, want a single array-of-string type", name, tags)
			}
			for _, want := range []string{
				"handoff", "lane:s", "lane:vision", "lane:auto", "size:xl", "repo:<owner/name>",
				"issue:<owner/repo#n>", "source:<harness>/<run>", "reply:cairn-comment", "reply:signal",
				"NOT provenance", "semi-trusted", "prompt injection",
			} {
				if !strings.Contains(tl.Description, want) {
					t.Errorf("%s description does not document %q", name, want)
				}
			}
		}
		if !found {
			t.Errorf("tool %s not listed", name)
		}
	}

	tags := []any{"handoff", "lane:auto", "size:s", "source:claude-code/brief-2026-09-11", "reply:cairn-comment", "handoff"}
	want := []string{"handoff", "lane:auto", "size:s", "source:claude-code/brief-2026-09-11", "reply:cairn-comment"}

	res := callTool(t, sess, "artifact_create", map[string]any{
		"body": "# task\n\n- do it", "title": "handoff", "share_type": "markdown",
		"model": "claude-opus-5", "tags": tags,
	})
	if res.IsError {
		t.Fatalf("artifact_create: %s", toolText(t, res))
	}
	var created mcpCreateOutput
	decodeToolJSON(t, res, &created)
	if !slices.Equal(created.Tags, want) {
		t.Fatalf("artifact_create tags = %q, want %q", created.Tags, want)
	}

	res = callTool(t, sess, "bundle_create", map[string]any{
		"title": "handoff bundle",
		"members": []map[string]any{
			{"name": "prompt.md", "body": "# task"},
			{"name": "context.log", "body": "log"},
		},
		"tags": tags,
	})
	if res.IsError {
		t.Fatalf("bundle_create: %s", toolText(t, res))
	}
	var bundle mcpBundleCreateOutput
	decodeToolJSON(t, res, &bundle)
	if !slices.Equal(bundle.Tags, want) {
		t.Fatalf("bundle_create tags = %q, want %q", bundle.Tags, want)
	}

	var readOut mcpReadOutput
	decodeToolJSON(t, callTool(t, sess, "artifact_read", map[string]any{"id": created.MCP}), &readOut)
	if !slices.Equal(readOut.Tags, want) {
		t.Fatalf("artifact_read tags = %q, want %q", readOut.Tags, want)
	}

	evs := recv.waitN(t, 2)
	for _, id := range []string{created.ID, bundle.ID} {
		ev, _ := eventFor(t, evs, id)
		if !slices.Equal(ev.Data.Tags, want) {
			t.Errorf("event %s tags = %q, want %q", id, ev.Data.Tags, want)
		}
		if ev.Data.OnBehalfOf != "claude-code/1.2.3" {
			t.Errorf("event %s on_behalf_of = %q, want the MCP client's initialize name/version", id, ev.Data.OnBehalfOf)
		}
	}
	if ev, _ := eventFor(t, evs, bundle.ID); ev.Data.ShareType != "bundle" {
		t.Errorf("bundle event share_type = %q", ev.Data.ShareType)
	}

	res = callTool(t, sess, "artifact_create", map[string]any{"body": "x", "tags": `["handoff","lane:s"]`})
	if res.IsError {
		t.Fatalf("artifact_create with string-encoded tags: %s", toolText(t, res))
	}
	var unwrapped mcpCreateOutput
	decodeToolJSON(t, res, &unwrapped)
	if !slices.Equal(unwrapped.Tags, []string{"handoff", "lane:s"}) {
		t.Fatalf("string-encoded tags stored as %q", unwrapped.Tags)
	}

	if bad := callTool(t, sess, "artifact_create", map[string]any{"body": "x", "tags": []any{"Bad Tag"}}); !bad.IsError {
		t.Fatalf("artifact_create with an invalid tag succeeded: %s", toolText(t, bad))
	}
}
