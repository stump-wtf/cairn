package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/sharetype"
	"github.com/stump-wtf/cairn/internal/store"
)

// The create path's remaining sites (#281): share type, title, checksum, the
// multipart envelope, and bundle members.
//
// Governing: ADR-0025, SPEC-0019 VE-1, VE-2, VE-3, VE-4, VE-6

func init() { guardSiteSources = append(guardSiteSources, createPathRemainingSites) }

// createPathRemainingSites are the #281 sites on the VE-6 migration guard.
func createPathRemainingSites(t *testing.T) []guardSite {
	s := New(nil, nil, nil, Config{MaxUploadBytes: 10}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := func(target string, header map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, target, nil)
		for k, v := range header {
			r.Header.Set(k, v)
		}
		return r
	}
	longTitle := strings.Repeat("t", artifact.MaxTitleBytes+1)
	members := func(names []string, tooLarge ...bool) func() error {
		return func() error {
			var m memberChecks
			for i, n := range names {
				m.add(n, i < len(tooLarge) && tooLarge[i], 10)
			}
			return m.err()
		}
	}
	// The store's own create paths, driven through their real entry points so a
	// regression inside validate (or at the checksum or member-size check) is
	// caught, not only one in the helpers they call. Every rejection below
	// happens before the store touches Postgres, so no pool is needed.
	st := store.New(nil, objectstore.NewMemory(), store.Options{MaxUploadBytes: 10})
	now := time.Now()
	prov := artifact.Provenance{ActorID: "u1", Channel: artifact.ChannelAPI, CapturedAt: now}
	access := artifact.AccessPolicy{OwnerID: "u1", Visibility: artifact.VisibilityLink}
	single := func(mutate func(*store.CreateArtifactInput)) func() error {
		return func() error {
			in := store.CreateArtifactInput{ShareType: artifact.TypeFile, Body: strings.NewReader("x"),
				Provenance: prov, Access: access, ExpiresAt: now.Add(time.Hour)}
			mutate(&in)
			_, err := st.CreateArtifact(context.Background(), in)
			return err
		}
	}
	bundle := func(title string, bodies map[string]string, names ...string) func() error {
		return func() error {
			in := store.CreateBundleInput{Title: title, Provenance: prov, Access: access, ExpiresAt: now.Add(time.Hour)}
			for _, n := range names {
				in.Members = append(in.Members, store.MemberInput{Name: n, Body: strings.NewReader(bodies[n] + "x")})
			}
			_, err := st.CreateBundle(context.Background(), in)
			return err
		}
	}
	return []guardSite{
		{"type: unknown (query)", func() error { _, err := s.requestedShareType(req("/?type=exotic", nil)); return err }},
		{"type: unknown (header)", func() error {
			_, err := s.requestedShareType(req("/", map[string]string{typeHeader: "exotic"}))
			return err
		}},
		{"type: bundle on the single-body path", func() error { _, err := s.requestedShareType(req("/?type=bundle", nil)); return err }},
		{"type: unknown (multipart query)", func() error { _, err := s.multipartShareType(req("/?type=exotic", nil)); return err }},
		{"type: missing (store)", single(func(in *store.CreateArtifactInput) { in.ShareType = "" })},
		{"type: unknown (store)", single(func(in *store.CreateArtifactInput) { in.ShareType = "exotic" })},
		{"type: bundle (store)", single(func(in *store.CreateArtifactInput) { in.ShareType = artifact.TypeBundle })},
		{"title: query", func() error { _, err := requestedTitle(req("/?title="+longTitle, nil)); return err }},
		{"title: header", func() error {
			_, err := requestedTitle(req("/", map[string]string{titleHeader: longTitle}))
			return err
		}},
		{"title: form field", func() error { _, err := readTitleField(strings.NewReader(longTitle)); return err }},
		{"title: store single body", single(func(in *store.CreateArtifactInput) { in.Title = longTitle })},
		{"title: store bundle", bundle(longTitle, nil, "a")},
		{"checksum: mismatch (store)", single(func(in *store.CreateArtifactInput) { in.ExpectedSHA256 = "00" })},
		{"multipart: missing boundary", errMissingBoundary},
		{"multipart: malformed body", func() error { return errMalformedMultipart(io.ErrUnexpectedEOF) }},
		{"multipart: no file parts", errNoFileParts},
		{"members: duplicate name", members([]string{"a", "a"})},
		{"members: over the upload cap", members([]string{"a", "b"}, false, true)},
		{"members: too many", members(make([]string, store.MaxBundleMembers+1))},
		{"members: store none", bundle("", nil)},
		{"members: store empty name", bundle("", nil, "a", "")},
		{"members: store duplicate name", bundle("", nil, "a", "a")},
		{"members: store too many", bundle("", nil, make([]string, store.MaxBundleMembers+1)...)},
		{"members: store member too large", bundle("", map[string]string{"b": strings.Repeat("b", 64)}, "a", "b")},
	}
}

// postExpect sends a create and decodes its error, failing unless the status
// is want.
func postExpect(t *testing.T, target string, header map[string]string, body io.Reader, want int) errorEnvelope {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set("Content-Type", "text/plain")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, want, b)
	}
	return decodeError(t, resp)
}

// wantViolation is the full expected shape of one violation. limit is compared
// after a JSON round trip, so a number is a float64.
type wantViolation struct {
	field  string
	loc    errs.Location
	reason errs.Reason
	limit  any
	unit   string
	value  string // "<none>" when no value may be echoed
}

func assertViolations(t *testing.T, env errorEnvelope, code errs.Code, want ...wantViolation) {
	t.Helper()
	if env.Error.Code != code {
		t.Fatalf("code = %q, want %q (%+v)", env.Error.Code, code, env.Error)
	}
	vs := env.Error.Violations
	if len(vs) != len(want) {
		t.Fatalf("violations = %+v, want %d", vs, len(want))
	}
	for i, w := range want {
		v := vs[i]
		if v.Field != w.field || v.Location != w.loc || v.Reason != w.reason ||
			v.Limit != w.limit || v.Unit != w.unit || valueOf(v) != w.value || v.Message == "" {
			t.Fatalf("violation %d = %+v (value %s), want %+v", i, v, valueOf(v), w)
		}
	}
	if len(want) == 1 {
		if msg := want[0].field + ": " + vs[0].Message; env.Error.Message != msg {
			t.Fatalf("message = %q, want %q", env.Error.Message, msg)
		}
	} else if !strings.HasPrefix(env.Error.Message, fmt.Sprintf("%d problems: ", len(want))) {
		t.Fatalf("message = %q, want it to count %d problems", env.Error.Message, len(want))
	}
}

func creatableTypes() string {
	var keys []string
	for _, st := range sharetype.Default().Types() {
		if st.Key() != artifact.TypeBundle {
			keys = append(keys, string(st.Key()))
		}
	}
	return strings.Join(keys, ", ")
}

func TestViolationUnknownShareType(t *testing.T) {
	srv := storelessServer(t, noRateLimit())

	env := postExpect(t, srv.URL+"/v1/artifacts?type=exotic", nil, strings.NewReader("x"), http.StatusBadRequest)
	assertViolations(t, env, errs.CodeValidation,
		wantViolation{"type", errs.LocQuery, errs.ReasonUnknownValue, creatableTypes(), "", "exotic"})
	if !strings.Contains(env.Error.Message, "expected ") || !strings.Contains(env.Error.Message, "markdown") {
		t.Fatalf("message = %q, want it to list the allowed types", env.Error.Message)
	}

	env = postExpect(t, srv.URL+"/v1/artifacts", map[string]string{typeHeader: "exotic"}, strings.NewReader("x"), http.StatusBadRequest)
	assertViolations(t, env, errs.CodeValidation,
		wantViolation{typeHeader, errs.LocHeader, errs.ReasonUnknownValue, creatableTypes(), "", "exotic"})
}

func TestViolationBundleTypeOnSingleBody(t *testing.T) {
	srv := storelessServer(t, noRateLimit())
	env := postExpect(t, srv.URL+"/v1/artifacts?type=bundle", nil, strings.NewReader("x"), http.StatusBadRequest)
	assertViolations(t, env, errs.CodeValidation,
		wantViolation{"type", errs.LocQuery, errs.ReasonNotAllowed, nil, "", "bundle"})
	if !strings.Contains(env.Error.Message, "multipart") {
		t.Fatalf("message = %q, want it to say how a bundle is made", env.Error.Message)
	}
}

func TestViolationTitleTooLong(t *testing.T) {
	srv := storelessServer(t, noRateLimit())
	long := strings.Repeat("t", artifact.MaxTitleBytes+1)
	echo := long[:errs.MaxValueBytes]
	limit := float64(artifact.MaxTitleBytes)

	env := postExpect(t, srv.URL+"/v1/artifacts?title="+long, nil, strings.NewReader("x"), http.StatusBadRequest)
	assertViolations(t, env, errs.CodeValidation,
		wantViolation{"title", errs.LocQuery, errs.ReasonTooLong, limit, errs.UnitBytes, echo})

	env = postExpect(t, srv.URL+"/v1/artifacts", map[string]string{titleHeader: long}, strings.NewReader("x"), http.StatusBadRequest)
	assertViolations(t, env, errs.CodeValidation,
		wantViolation{titleHeader, errs.LocHeader, errs.ReasonTooLong, limit, errs.UnitBytes, echo})

	// Multipart: the form field is rejected whole, before any file spools.
	body, ct := multipartBody(t, map[string]string{"title": long}, file{"a.md", "x"})
	env = postExpect(t, srv.URL+"/v1/artifacts", map[string]string{"Content-Type": ct}, body, http.StatusBadRequest)
	assertViolations(t, env, errs.CodeValidation,
		wantViolation{"title", errs.LocForm, errs.ReasonTooLong, limit, errs.UnitBytes, echo})
}

// Every create-header problem is reported together (VE-4).
func TestViolationTypeTitleTTLTogether(t *testing.T) {
	srv := storelessServer(t, noRateLimit())
	env := postExpect(t, srv.URL+"/v1/artifacts?type=bundle&title="+strings.Repeat("t", artifact.MaxTitleBytes+1),
		map[string]string{ttlHeader: "7d"}, strings.NewReader("x"), http.StatusBadRequest)
	vs := env.Error.Violations
	if len(vs) != 3 || vs[0].Field != ttlHeader || vs[1].Field != "type" || vs[2].Field != "title" {
		t.Fatalf("violations = %+v, want the TTL, type and title", vs)
	}
}

type file struct{ name, body string }

func multipartBody(t *testing.T, fields map[string]string, files ...file) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, f.name))
		h.Set("Content-Type", "text/plain")
		fw, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write([]byte(f.body))
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func TestViolationMultipartEnvelope(t *testing.T) {
	srv := storelessServer(t, noRateLimit())

	env := postExpect(t, srv.URL+"/v1/artifacts", map[string]string{"Content-Type": "multipart/form-data"}, strings.NewReader("x"), http.StatusBadRequest)
	assertViolations(t, env, errs.CodeValidation,
		wantViolation{"Content-Type", errs.LocHeader, errs.ReasonInvalidFormat, nil, "", "<none>"})

	// A part whose header cannot be parsed, and a file part cut off before its
	// closing boundary: both are the client's malformed body, not a 500.
	for name, raw := range map[string]string{
		"bad part header": "--zz\r\nno colon here\r\n\r\nx\r\n--zz--\r\n",
		"truncated part":  "--zz\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.md\"\r\n\r\nunfinished",
	} {
		resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts", "alice", strings.NewReader(raw), "multipart/form-data; boundary=zz")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", name, resp.StatusCode)
		}
		assertViolations(t, decodeError(t, resp), errs.CodeValidation,
			wantViolation{"body", errs.LocBody, errs.ReasonInvalidFormat, nil, "", "<none>"})
	}

	body, ct := multipartBody(t, map[string]string{"title": "no files"})
	env = postExpect(t, srv.URL+"/v1/artifacts", map[string]string{"Content-Type": ct}, body, http.StatusBadRequest)
	assertViolations(t, env, errs.CodeValidation,
		wantViolation{"file", errs.LocForm, errs.ReasonRequired, nil, "", "<none>"})

	body, ct = multipartBody(t, nil, file{"a.md", "x"})
	env = postExpect(t, srv.URL+"/v1/artifacts?type=exotic", map[string]string{"Content-Type": ct}, body, http.StatusBadRequest)
	assertViolations(t, env, errs.CodeValidation,
		wantViolation{"type", errs.LocQuery, errs.ReasonUnknownValue, creatableTypes(), "", "exotic"})
}

// A lone multipart file over the cap is the body's 413 (VE-1 "Oversize body").
func TestViolationMultipartSingleFileOversize(t *testing.T) {
	cfg := noRateLimit()
	cfg.MaxUploadBytes = 8
	srv := storelessServer(t, cfg)
	body, ct := multipartBody(t, nil, file{"a.md", strings.Repeat("a", 64)})
	env := postExpect(t, srv.URL+"/v1/artifacts", map[string]string{"Content-Type": ct}, body, http.StatusRequestEntityTooLarge)
	assertViolations(t, env, errs.CodePayloadTooLarge,
		wantViolation{"body", errs.LocBody, errs.ReasonTooLarge, float64(8), errs.UnitBytes, "<none>"})
}

// VE-4: a bundle with two bad members reports both. Two oversize members are
// a 413; a duplicate name among them makes it a 400.
func TestViolationBundleMembersCumulative(t *testing.T) {
	cfg := noRateLimit()
	cfg.MaxUploadBytes = 8
	srv := storelessServer(t, cfg)
	big := strings.Repeat("a", 64)

	body, ct := multipartBody(t, nil, file{"a.md", big}, file{"b.md", "ok"}, file{"c.md", big})
	env := postExpect(t, srv.URL+"/v1/artifacts", map[string]string{"Content-Type": ct}, body, http.StatusRequestEntityTooLarge)
	assertViolations(t, env, errs.CodePayloadTooLarge,
		wantViolation{"members[0].content", errs.LocForm, errs.ReasonTooLarge, float64(8), errs.UnitBytes, "<none>"},
		wantViolation{"members[2].content", errs.LocForm, errs.ReasonTooLarge, float64(8), errs.UnitBytes, "<none>"})

	body, ct = multipartBody(t, nil, file{"a.md", "ok"}, file{"a.md", "ok"}, file{"c.md", big})
	env = postExpect(t, srv.URL+"/v1/artifacts", map[string]string{"Content-Type": ct}, body, http.StatusBadRequest)
	assertViolations(t, env, errs.CodeValidation,
		wantViolation{"members[1].name", errs.LocForm, errs.ReasonDuplicate, nil, "", "a.md"},
		wantViolation{"members[2].content", errs.LocForm, errs.ReasonTooLarge, float64(8), errs.UnitBytes, "<none>"})
}

func TestViolationBundleTooManyMembers(t *testing.T) {
	srv := storelessServer(t, noRateLimit())
	files := make([]file, store.MaxBundleMembers+1)
	for i := range files {
		files[i] = file{fmt.Sprintf("f%d.md", i), "x"}
	}
	body, ct := multipartBody(t, nil, files...)
	env := postExpect(t, srv.URL+"/v1/artifacts", map[string]string{"Content-Type": ct}, body, http.StatusBadRequest)
	assertViolations(t, env, errs.CodeValidation,
		wantViolation{"members", errs.LocForm, errs.ReasonTooMany, float64(store.MaxBundleMembers), errs.UnitCount, "<none>"})
}

// A checksum mismatch names the header, echoes the digest the caller sent,
// and still matches errs.ErrChecksumMismatch in the store.
func TestIntegrationViolationChecksumMismatch(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20})
	sum := sha256.Sum256([]byte("something else"))
	sent := hex.EncodeToString(sum[:])

	env := postExpect(t, srv.URL+"/v1/artifacts", map[string]string{store.ChecksumHeader: sent},
		strings.NewReader("the body"), http.StatusBadRequest)
	assertViolations(t, env, errs.CodeValidation,
		wantViolation{store.ChecksumHeader, errs.LocHeader, errs.ReasonChecksum, nil, "", sent})
}

// The multipart bundle path creates normally when every member is valid, so
// the member checks reject only what they should.
func TestIntegrationBundleMembersValidStillCreates(t *testing.T) {
	srv := testServer(t, noRateLimit(), store.Options{MaxUploadBytes: 1 << 20})
	body, ct := multipartBody(t, map[string]string{"title": "two"}, file{"a.md", "# a"}, file{"b.md", "# b"})
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts", "alice", body, ct)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if art := decodeArtifact(t, resp); art.ShareType != artifact.TypeBundle || art.Title != "two" {
		t.Fatalf("artifact = %+v, want a bundle titled two", art)
	}
}
