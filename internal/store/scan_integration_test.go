package store

// Ingest Secret Scan Store Tests
//
// SPEC-0017 at the create paths: a masked body is never stored raw, a rejected
// write leaves nothing behind, a bundle rejection names its member, a writer
// can downgrade but not disable, binary bodies are left alone, the scan cap
// rejects or (opt-in) stores unscanned with a WARN, a masked upload's declared
// checksum is accepted, and a scan that fails fails the create closed. Every
// test plants a token built at run time from split literals and greps every
// output it can reach: the stored objects, the rows, the error, the log and
// the metric exposition.
//
// Governing: ADR-0023, SPEC-0017 RD-1, RD-4, RD-5, RD-6, RD-7, RD-10, RD-11,
// REQ "Error Handling Standards"
//
// @joestump 09/26/2026 - Added for cairn#292.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/metrics"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/redact"
)

// scanFixture is a store over an enumerable in-memory object store, with its
// event, log and metric outputs in reach.
type scanFixture struct {
	s      *Store
	pool   *pgxpool.Pool
	mem    *objectstore.Memory
	events *tagCaptureEmitter
	logs   *lockedBuffer
	reg    *metrics.Registry
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// newScanFixture builds the fixture. o's Scanner, when set, replaces the
// default one; everything else the fixture owns.
func newScanFixture(t *testing.T, o Options) *scanFixture {
	t.Helper()
	_, pool := newTestStore(t, Options{}) // skips without CAIRN_TEST_DATABASE_URL
	f := &scanFixture{pool: pool, mem: objectstore.NewMemory(), events: &tagCaptureEmitter{}, logs: &lockedBuffer{}, reg: metrics.New()}
	o = withScanner(t, o)
	o.Emitter = f.events
	o.Metrics = f.reg
	o.Logger = slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.s = New(pool, f.mem, o)
	return f
}

// objects returns every stored object's key and bytes under prefix.
func (f *scanFixture) objects(t *testing.T, prefix string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, k := range f.mem.KeysWithPrefix(prefix) {
		rc, err := f.mem.Get(context.Background(), k)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[k] = string(b)
	}
	return out
}

// assertNothingStored: no artifact or member row, no object at all (staging
// included), and no creation event.
func (f *scanFixture) assertNothingStored(t *testing.T) {
	t.Helper()
	var rows int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM artifacts) + (SELECT count(*) FROM bundle_members) + (SELECT count(*) FROM blobs)`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d artifact, member or blob rows exist after a rejected create", rows)
	}
	if keys := f.mem.KeysWithPrefix(""); len(keys) != 0 {
		t.Errorf("objects left after a rejected create: %v", keys)
	}
	if ev := f.events.snapshot(); len(ev) != 0 {
		t.Errorf("a rejected create emitted %d artifact.created events", len(ev))
	}
}

// assertNoTokenAnywhere greps every object, every artifact and member row, the
// log and the metric exposition for any part of the tokens.
func (f *scanFixture) assertNoTokenAnywhere(t *testing.T, tokens ...string) {
	t.Helper()
	outputs := map[string]string{"log": f.logs.String(), "metrics": exposition(t, f.reg)}
	for k, v := range f.objects(t, "") {
		outputs["object "+k] = v
	}
	for _, table := range []string{"artifacts", "bundle_members", "blobs"} {
		var dump string
		if err := f.pool.QueryRow(context.Background(),
			`SELECT COALESCE(string_agg(row_to_json(t)::text, E'\n'), '') FROM `+table+` t`).Scan(&dump); err != nil {
			t.Fatal(err)
		}
		outputs["table "+table] = dump
	}
	for where, out := range outputs {
		assertNoPart(t, where, out, tokens...)
	}
}

func assertNoPart(t *testing.T, where, out string, tokens ...string) {
	t.Helper()
	for _, tok := range tokens {
		if strings.Contains(out, tok) || strings.Contains(out, tok[4:24]) {
			t.Errorf("%s holds part of a planted token", where)
		}
	}
}

// exposition renders a registry's metrics as text.
func exposition(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	mfs, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, mf := range mfs {
		b.WriteString(mf.String())
	}
	return b.String()
}

// redactions reads one cairn_redactions_total series.
func redactions(t *testing.T, reg *metrics.Registry, surface metrics.RedactionSurface, outcome metrics.RedactionOutcome) float64 {
	t.Helper()
	mfs, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "cairn_redactions_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["surface"] == string(surface) && labels["outcome"] == string(outcome) {
				return m.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("no series for %s/%s", surface, outcome)
	return 0
}

func typed(body string, shareType artifact.ShareType, media string) CreateArtifactInput {
	in := input([]byte(body))
	in.ShareType = shareType
	in.DeclaredMediaType = media
	return in
}

// largeText pads line up past the in-memory scan size with filler lines, so
// the body is scanned back from staging in windows.
func largeText(line string) string {
	filler := strings.Repeat("filler line without any credential in it\n", (scanInMemoryBytes/40)+64)
	return filler + line + "\n" + filler
}

// TestScanMaskedBodyNeverStoredRaw: SPEC-0017 RD-1 "Masked body never stored
// raw", for a body scanned from memory and one scanned back from staging. The
// promoted blob holds [REDACTED], no object holds the token, the stored
// SHA-256 and size are the masked bytes', and nothing is left in staging.
func TestScanMaskedBodyNeverStoredRaw(t *testing.T) {
	for _, tc := range []struct {
		name string
		body func(tok string) string
	}{
		{"in memory", func(tok string) string { return "# deploy\n\nuse " + tok + " to push\n" }},
		{"from staging", func(tok string) string { return largeText("use " + tok + " to push") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newScanFixture(t, Options{})
			tok := plantedToken(3)
			art, err := f.s.CreateArtifact(context.Background(), typed(tc.body(tok), "markdown", "text/markdown"))
			if err != nil {
				t.Fatal(err)
			}
			if art.Redaction.Status != redact.StatusMasked || art.Redaction.Count != 1 || art.Redaction.Rules["github-pat"] != 1 {
				t.Errorf("outcome = %+v, want masked, 1, github-pat", art.Redaction)
			}
			blobs := f.objects(t, "blobs/")
			if len(blobs) != 1 {
				t.Fatalf("%d promoted blobs, want 1", len(blobs))
			}
			stored := blobs[shardedKey(art.BodySHA256)]
			if !strings.Contains(stored, redact.Mask) {
				t.Error("the promoted blob does not hold the mask")
			}
			if sha256Hex([]byte(stored)) != art.BodySHA256 || int64(len(stored)) != art.Size {
				t.Error("the recorded SHA-256 and size are not the stored bytes'")
			}
			if strings.Replace(tc.body(tok), tok, redact.Mask, 1) != stored {
				t.Error("masking changed more than the token")
			}
			assertNoStagingLeak(t, f.mem)
			if got := outcomeOf(t, f.pool, art.ID); got.Status != redact.StatusMasked || got.Count != 1 {
				t.Errorf("recorded outcome = %+v", got)
			}
			if n := redactions(t, f.reg, metrics.SurfaceArtifact, metrics.OutcomeMasked); n != 1 {
				t.Errorf("masked scans counted = %v, want 1", n)
			}
			f.assertNoTokenAnywhere(t, tok)
		})
	}
}

// TestScanRejectedWriteLeavesNothing: SPEC-0017 RD-1 "Rejected write leaves
// nothing" and RD-11. A code artifact holding a token is refused by default
// with a secret_detected violation carrying the rule, line and column; no row,
// no object (staging included) and no event exist afterwards, and neither the
// error nor its rendered violation holds the token.
func TestScanRejectedWriteLeavesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		body func(tok string) string
		line int
	}{
		{"in memory", func(tok string) string { return "package main\n\nconst key = \"" + tok + "\"\n" }, 3},
		{"from staging", func(tok string) string { return largeText("const key = \"" + tok + "\"") }, (scanInMemoryBytes/40 + 64) + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newScanFixture(t, Options{})
			tok := plantedToken(4)
			_, err := f.s.CreateArtifact(context.Background(), typed(tc.body(tok), "code", "text/x-go"))
			if !errors.Is(err, errs.ErrValidation) || !errors.Is(err, redact.ErrSecretDetected) {
				t.Fatalf("err = %v, want a secret_detected validation error", err)
			}
			vs := errs.ViolationsOf(err)
			if len(vs) != 1 {
				t.Fatalf("violations = %+v, want 1", vs)
			}
			v := vs[0]
			if v.Field != "body" || v.Location != errs.LocBody || v.Reason != errs.ReasonSecretDetected || v.Value != nil {
				t.Errorf("violation = %+v", v)
			}
			if v.Extra["rule"] != "github-pat" || v.Extra["line"] != tc.line || v.Extra["column"] != 14 {
				t.Errorf("violation location = %v, want github-pat at line %d, column 14", v.Extra, tc.line)
			}
			if !strings.Contains(v.Message, "--redact=mask") {
				t.Errorf("message %q does not name the downgrade", v.Message)
			}
			rendered, _ := json.Marshal(vs)
			assertNoPart(t, "violation JSON", string(rendered), tok)
			assertNoPart(t, "error string", err.Error(), tok)
			f.assertNothingStored(t)
			if n := redactions(t, f.reg, metrics.SurfaceArtifact, metrics.OutcomeRejected); n != 1 {
				t.Errorf("rejected scans counted = %v, want 1", n)
			}
			f.assertNoTokenAnywhere(t, tok)
		})
	}
}

func scanBundle(members ...MemberInput) CreateBundleInput {
	return CreateBundleInput{
		Title:      "bundle",
		Members:    members,
		Provenance: artifact.Provenance{ActorID: "u1", Channel: artifact.ChannelCLI, CapturedAt: time.Now()},
		Access:     artifact.AccessPolicy{OwnerID: "u1", Visibility: artifact.VisibilityLink},
		ExpiresAt:  time.Now().Add(time.Hour),
	}
}

func member(name, body string) MemberInput {
	return MemberInput{Name: name, Body: strings.NewReader(body), DeclaredMediaType: "text/plain"}
}

// TestScanBundleRejectionNamesMember: SPEC-0017 RD-4 "Bundle rejection names
// the member". The third member's token rejects the whole bundle under
// members[2].content, with the rule; every member with a secret is named, and
// nothing is stored.
func TestScanBundleRejectionNamesMember(t *testing.T) {
	f := newScanFixture(t, Options{})
	tok, tok2 := plantedToken(5), plantedToken(6)
	_, err := f.s.CreateBundle(context.Background(), scanBundle(
		member("a.txt", "first"),
		member("b.txt", "second"),
		member("c.env", "TOKEN="+tok+"\n"),
		member("d.txt", "fourth"),
		member("e.env", "\n\nTOKEN="+tok2+"\n"),
	))
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("err = %v, want validation_failed", err)
	}
	vs := errs.ViolationsOf(err)
	if len(vs) != 2 {
		t.Fatalf("violations = %+v, want 2", vs)
	}
	for i, want := range []struct {
		field string
		line  int
	}{{"members[2].content", 1}, {"members[4].content", 3}} {
		if vs[i].Field != want.field || vs[i].Reason != errs.ReasonSecretDetected || vs[i].Extra["rule"] != "github-pat" || vs[i].Extra["line"] != want.line {
			t.Errorf("violation %d = %+v, want %s line %d", i, vs[i], want.field, want.line)
		}
	}
	var rej *redact.Rejection
	if !errors.As(err, &rej) || rej.Field != "members[2].content" {
		t.Errorf("the rejection in the chain names %q, want members[2].content", rej.Field)
	}
	f.assertNothingStored(t)
	if n := redactions(t, f.reg, metrics.SurfaceBundle, metrics.OutcomeRejected); n != 2 {
		t.Errorf("rejected bundle scans counted = %v, want 2", n)
	}
	f.assertNoTokenAnywhere(t, tok, tok2)
	assertNoPart(t, "error string", err.Error(), tok, tok2)
}

// TestScanBundleMasksWithDowngrade: a downgraded bundle stores each member
// masked, records each member's outcome and the bundle's total, and promotes
// no raw member.
func TestScanBundleMasksWithDowngrade(t *testing.T) {
	f := newScanFixture(t, Options{})
	tok := plantedToken(7)
	in := scanBundle(member("a.txt", "clean"), member("b.env", "TOKEN="+tok+"\n"))
	in.RedactionDowngrade = true
	art, err := f.s.CreateBundle(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if art.Redaction.Status != redact.StatusMasked || art.Redaction.Count != 1 {
		t.Errorf("bundle outcome = %+v, want masked, 1", art.Redaction)
	}
	rows, err := f.pool.Query(context.Background(),
		`SELECT m.redaction_status, m.redaction_count, b.size_bytes FROM bundle_members m JOIN blobs b ON b.sha256 = m.blob_sha256 WHERE m.bundle_id = $1 ORDER BY m.ordinal`, art.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	var total int64
	for rows.Next() {
		var st string
		var n int
		var size int64
		if err := rows.Scan(&st, &n, &size); err != nil {
			t.Fatal(err)
		}
		got = append(got, st)
		total += size
	}
	if strings.Join(got, ",") != "clean,masked" {
		t.Errorf("member outcomes = %v, want clean,masked", got)
	}
	if total != art.Size {
		t.Errorf("bundle size %d is not the stored members' %d", art.Size, total)
	}
	assertNoStagingLeak(t, f.mem)
	f.assertNoTokenAnywhere(t, tok)
}

// TestScanDowngradeStoresMaskedCode: SPEC-0017 RD-5 "Downgrade stores masked
// code".
func TestScanDowngradeStoresMaskedCode(t *testing.T) {
	f := newScanFixture(t, Options{})
	tok := plantedToken(8)
	in := typed("const key = \""+tok+"\"\n", "code", "text/x-go")
	in.RedactionDowngrade = true
	art, err := f.s.CreateArtifact(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !art.Redaction.Redacted() || art.Redaction.Count != 1 {
		t.Errorf("outcome = %+v, want masked", art.Redaction)
	}
	f.assertNoTokenAnywhere(t, tok)
}

// TestScanRejectTypesConfigurable: CAIRN_REDACTION_REJECT_TYPES decides which
// share types reject. With markdown listed, markdown rejects; with an empty
// list, code masks.
func TestScanRejectTypesConfigurable(t *testing.T) {
	tok := plantedToken(9)
	strict := newScanFixture(t, Options{RedactionRejectTypes: []string{"markdown"}})
	if _, err := strict.s.CreateArtifact(context.Background(), typed("x "+tok, "markdown", "text/markdown")); !errors.Is(err, redact.ErrSecretDetected) {
		t.Errorf("markdown under a markdown reject list: err = %v, want a rejection", err)
	}
	lax := newScanFixture(t, Options{RedactionRejectTypes: []string{}})
	art, err := lax.s.CreateArtifact(context.Background(), typed("x "+tok, "code", "text/plain"))
	if err != nil || !art.Redaction.Redacted() {
		t.Errorf("code under an empty reject list: %v, %+v, want masked", err, art)
	}
}

// TestScanClassifiedCodeRejects: SPEC-0017 RD-4. An upload declared as the
// generic file type whose media type names a language is stored as code, so
// it rejects as code does, title included, and leaves nothing. This is how
// `cairn add main.go` arrives. A plain-text file still masks.
func TestScanClassifiedCodeRejects(t *testing.T) {
	f := newScanFixture(t, Options{})
	tok := plantedToken(16)

	_, err := f.s.CreateArtifact(context.Background(), typed("package main\n\nconst key = \""+tok+"\"\n", artifact.TypeFile, "text/x-go"))
	if vs := errs.ViolationsOf(err); !errors.Is(err, redact.ErrSecretDetected) || len(vs) != 1 || vs[0].Field != "body" {
		t.Fatalf("a Go file shared as file: err = %v, want a secret_detected violation on body", err)
	}
	in := typed("package main\n", artifact.TypeFile, "text/x-go")
	in.Title = "key " + tok
	_, err = f.s.CreateArtifact(context.Background(), in)
	if vs := errs.ViolationsOf(err); len(vs) != 1 || vs[0].Field != "title" {
		t.Fatalf("a Go file's title: err = %v, want a secret_detected violation on title", err)
	}
	f.assertNothingStored(t)

	art, err := f.s.CreateArtifact(context.Background(), typed("note "+tok+"\n", artifact.TypeFile, "text/plain"))
	if err != nil || !art.Redaction.Redacted() {
		t.Fatalf("a text file: %v, %+v, want masked", err, art)
	}
	f.assertNoTokenAnywhere(t, tok)
}

// TestScanTitle: a title is scanned with its surface's mode. A code
// artifact's title with a token is refused, naming the title; a markdown
// artifact's is stored masked and counted in the outcome.
func TestScanTitle(t *testing.T) {
	f := newScanFixture(t, Options{})
	tok := plantedToken(10)

	in := typed("fine", "code", "text/plain")
	in.Title = "key " + tok
	_, err := f.s.CreateArtifact(context.Background(), in)
	if vs := errs.ViolationsOf(err); len(vs) != 1 || vs[0].Field != "title" || vs[0].Reason != errs.ReasonSecretDetected {
		t.Fatalf("code title: err = %v, want a secret_detected violation on title", err)
	}
	f.assertNothingStored(t)

	in = typed("fine", "markdown", "text/markdown")
	in.Title = "key " + tok
	art, err := f.s.CreateArtifact(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if art.Title != "key "+redact.Mask || art.Redaction.Status != redact.StatusMasked || art.Redaction.Count != 1 {
		t.Errorf("markdown title = %q, outcome %+v", art.Title, art.Redaction)
	}
	f.assertNoTokenAnywhere(t, tok)
}

// TestScanBinaryContent: SPEC-0017 RD-6. A body declared image/png that is
// UTF-8 text is scanned; a real PNG is stored byte for byte, not scanned, and
// recorded not_scanned_binary, even though a token-shaped string sits inside it.
func TestScanBinaryContent(t *testing.T) {
	f := newScanFixture(t, Options{})
	tok := plantedToken(11)

	art, err := f.s.CreateArtifact(context.Background(), typed("not an image: "+tok+"\n", artifact.TypeFile, "image/png"))
	if err != nil {
		t.Fatal(err)
	}
	if art.Redaction.Status != redact.StatusMasked {
		t.Errorf("text declared image/png: outcome %+v, want masked", art.Redaction)
	}
	f.assertNoTokenAnywhere(t, tok)

	png := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89"), []byte("tEXt"+tok)...)
	png = append(png, 0x00, 0xff, 0xfe)
	art, err = f.s.CreateArtifact(context.Background(), CreateArtifactInput{
		ShareType: artifact.TypeFile, Body: bytes.NewReader(png), DeclaredMediaType: "image/png",
		Provenance: artifact.Provenance{ActorID: "u1", Channel: artifact.ChannelCLI, CapturedAt: time.Now()},
		Access:     artifact.AccessPolicy{OwnerID: "u1", Visibility: artifact.VisibilityLink},
		ExpiresAt:  time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if art.Redaction.Status != redact.StatusNotScannedBinary {
		t.Errorf("real PNG: outcome %+v, want not_scanned_binary", art.Redaction)
	}
	if art.BodySHA256 != sha256Hex(png) || f.objects(t, "blobs/")[shardedKey(art.BodySHA256)] != string(png) {
		t.Error("the PNG was not stored unchanged")
	}
	if n := redactions(t, f.reg, metrics.SurfaceArtifact, metrics.OutcomeNotScannedBinary); n != 1 {
		t.Errorf("binary bodies counted = %v, want 1", n)
	}
}

// TestScanOversize: SPEC-0017 RD-7. A 20 MiB text body is refused by default
// with too_large_to_scan and the 16 MiB limit, leaving nothing. Under
// store_unscanned it is stored with status not_scanned_oversize, counted, and
// announced by a WARN carrying the artifact id and size and no content.
func TestScanOversize(t *testing.T) {
	const size = 20 << 20
	tok := plantedToken(12)
	body := func() string { return tok + "\n" + strings.Repeat("a", size-len(tok)-1) }

	f := newScanFixture(t, Options{MaxUploadBytes: 32 << 20})
	_, err := f.s.CreateArtifact(context.Background(), typed(body(), "markdown", "text/plain"))
	vs := errs.ViolationsOf(err)
	if !errors.Is(err, redact.ErrTooLargeToScan) || len(vs) != 1 {
		t.Fatalf("err = %v, want one too_large_to_scan violation", err)
	}
	if vs[0].Field != "body" || vs[0].Reason != errs.ReasonTooLargeToScan || vs[0].Limit != int64(16777216) || vs[0].Unit != errs.UnitBytes {
		t.Errorf("violation = %+v, want body too_large_to_scan limit 16777216 bytes", vs[0])
	}
	f.assertNothingStored(t)
	if n := redactions(t, f.reg, metrics.SurfaceArtifact, metrics.OutcomeTooLarge); n != 1 {
		t.Errorf("too-large scans counted = %v, want 1", n)
	}

	sc, err := redact.New(redact.Config{Oversize: redact.OversizeStoreUnscanned})
	if err != nil {
		t.Fatal(err)
	}
	f = newScanFixture(t, Options{MaxUploadBytes: 32 << 20, Scanner: sc})
	art, err := f.s.CreateArtifact(context.Background(), typed(body(), "markdown", "text/plain"))
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(t, f.pool, art.ID); got.Status != redact.StatusNotScannedOversize {
		t.Errorf("recorded status = %s, want not_scanned_oversize", got.Status)
	}
	if n := redactions(t, f.reg, metrics.SurfaceArtifact, metrics.OutcomeNotScannedOversize); n != 1 {
		t.Errorf("unscanned stores counted = %v, want 1", n)
	}
	logs := f.logs.String()
	for _, want := range []string{"level=WARN", "artifact=" + art.PublicID, "field=body", "size_bytes=20971520", "CAIRN_REDACTION_OVERSIZE=store_unscanned"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q: %s", want, logs)
		}
	}
	assertNoPart(t, "log", logs, tok)
	if strings.Contains(logs, "aaaaaaaa") {
		t.Error("the WARN carries content")
	}
}

// TestScanDeclaredChecksumOfMaskedUpload: SPEC-0017 RD-10. The client's
// SHA-256 of the raw body is verified before masking and accepted; the
// artifact carries the masked body's SHA-256 and redacted. Dedup keys on the
// stored bytes: two bodies that differ only in their token store one blob.
func TestScanDeclaredChecksumOfMaskedUpload(t *testing.T) {
	f := newScanFixture(t, Options{})
	tokA, tokB := plantedToken(13), plantedToken(14)
	raw := "# notes\n\ntoken " + tokA + "\n"

	in := typed(raw, "markdown", "text/markdown")
	in.ExpectedSHA256 = sha256Hex([]byte(raw))
	a, err := f.s.CreateArtifact(context.Background(), in)
	if err != nil {
		t.Fatalf("declared checksum of a masked upload refused: %v", err)
	}
	masked := strings.Replace(raw, tokA, redact.Mask, 1)
	if a.BodySHA256 != sha256Hex([]byte(masked)) || a.BodySHA256 == in.ExpectedSHA256 || !a.Redaction.Redacted() {
		t.Errorf("sha %s redacted %v, want the masked body's sha and redacted", a.BodySHA256, a.Redaction.Redacted())
	}

	in = typed(raw, "markdown", "text/markdown")
	in.ExpectedSHA256 = sha256Hex([]byte("something else"))
	if _, err := f.s.CreateArtifact(context.Background(), in); !errors.Is(err, errs.ErrChecksumMismatch) {
		t.Errorf("a wrong declared checksum: err = %v, want checksum_mismatch", err)
	}

	b, err := f.s.CreateArtifact(context.Background(), typed(strings.Replace(raw, tokA, tokB, 1), "markdown", "text/markdown"))
	if err != nil {
		t.Fatal(err)
	}
	if b.BodySHA256 != a.BodySHA256 || len(f.objects(t, "blobs/")) != 1 {
		t.Error("two bodies that mask to the same bytes did not dedup to one blob")
	}
	f.assertNoTokenAnywhere(t, tokA, tokB)
}

// failingGets fails every read of a staging object, so a scan back from
// staging fails.
type failingGets struct{ *objectstore.Memory }

func (f failingGets) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if strings.HasPrefix(key, "staging/") {
		return nil, errors.New("object store unavailable")
	}
	return f.Memory.Get(ctx, key)
}

// TestScanFailureFailsClosed: SPEC-0017 "Detector error fails closed". A scan
// that cannot read the staged body, and a store with no scanner at all, fail
// the create as internal errors, store nothing and emit nothing.
func TestScanFailureFailsClosed(t *testing.T) {
	f := newScanFixture(t, Options{})
	f.s.obj = failingGets{f.mem}
	tok := plantedToken(15)
	_, err := f.s.CreateArtifact(context.Background(), typed(largeText(tok), "markdown", "text/plain"))
	if errs.CodeOf(err) != errs.CodeInternal || !errors.Is(err, redact.ErrScanFailed) {
		t.Fatalf("err = %v (%s), want an internal scan failure", err, errs.CodeOf(err))
	}
	f.assertNothingStored(t)
	if n := redactions(t, f.reg, metrics.SurfaceArtifact, metrics.OutcomeFailed); n != 1 {
		t.Errorf("failed scans counted = %v, want 1", n)
	}
	assertNoPart(t, "error", err.Error(), tok)

	_, pool := newTestStore(t, Options{})
	events := &tagCaptureEmitter{}
	mem := objectstore.NewMemory()
	bare := New(pool, mem, Options{Emitter: events})
	for name, create := range map[string]func() error{
		"artifact": func() error { _, err := bare.CreateArtifact(context.Background(), input([]byte("hello"))); return err },
		"bundle": func() error {
			_, err := bare.CreateBundle(context.Background(), scanBundle(member("a.txt", "a")))
			return err
		},
	} {
		if err := create(); errs.CodeOf(err) != errs.CodeInternal || !errors.Is(err, redact.ErrScanFailed) {
			t.Errorf("%s create without a scanner: err = %v, want an internal scan failure", name, err)
		}
	}
	if keys := mem.KeysWithPrefix(""); len(keys) != 0 || len(events.snapshot()) != 0 {
		t.Errorf("a store without a scanner stored %v or emitted events", keys)
	}
}

// TestScanConcurrentCreates: SPEC-0017 "Concurrent scans". 32 creates, each
// holding a token, run at once through the shared scanner: every markdown one
// is masked, every code one refused, and (under make ci's -race) nothing races.
func TestScanConcurrentCreates(t *testing.T) {
	f := newScanFixture(t, Options{})
	const n = 32
	var wg sync.WaitGroup
	errc := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok := plantedToken(100 + i)
			shareType := artifact.ShareType("markdown")
			if i%2 == 1 {
				shareType = "code"
			}
			art, err := f.s.CreateArtifact(context.Background(), typed("line\nkey "+tok+"\n", shareType, "text/plain"))
			switch {
			case shareType == "code" && !errors.Is(err, redact.ErrSecretDetected):
				errc <- errors.New("a code create was not refused")
			case shareType == "markdown" && (err != nil || !art.Redaction.Redacted()):
				errc <- errors.New("a markdown create was not masked")
			}
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	var tokens []string
	for i := range n {
		tokens = append(tokens, plantedToken(100+i))
	}
	f.assertNoTokenAnywhere(t, tokens...)
}
