package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/sharetype"
)

// validInput is a minimally-complete single-body create request for exercising
// validate() in isolation (no object store or database involved).
func validCreateInput() CreateArtifactInput {
	return CreateArtifactInput{
		ShareType:  artifact.TypeFile,
		Body:       strings.NewReader("x"),
		Provenance: artifact.Provenance{ActorID: "u1", Channel: artifact.ChannelCLI, CapturedAt: time.Unix(1, 0)},
		Access:     artifact.AccessPolicy{OwnerID: "u1", Visibility: artifact.VisibilityLink},
		ExpiresAt:  time.Unix(100, 0),
	}
}

// What the caller controls is a violation; what the server derives is an
// internal error (Governing: ADR-0025, SPEC-0019 VE-1, VE-6).
func TestCreateArtifactInputValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CreateArtifactInput)
		// want is the violation's field/reason, "internal" for an internal
		// error, or "" for none.
		want string
	}{
		{"valid", func(*CreateArtifactInput) {}, ""},
		{"missing share type", func(in *CreateArtifactInput) { in.ShareType = "" }, "type/required"},
		{"unknown share type", func(in *CreateArtifactInput) { in.ShareType = "some-future-type" }, "type/unknown_value"},
		{"bundle rejected on single path", func(in *CreateArtifactInput) { in.ShareType = artifact.TypeBundle }, "type/not_allowed"},
		{"title too long", func(in *CreateArtifactInput) { in.Title = strings.Repeat("t", artifact.MaxTitleBytes+1) }, "title/too_long"},
		{"title at the cap", func(in *CreateArtifactInput) { in.Title = strings.Repeat("t", artifact.MaxTitleBytes) }, ""},
		{"missing channel", func(in *CreateArtifactInput) { in.Provenance.Channel = "" }, "internal"},
		{"missing actor", func(in *CreateArtifactInput) { in.Provenance.ActorID = "" }, "internal"},
		{"missing owner", func(in *CreateArtifactInput) { in.Access.OwnerID = "" }, "internal"},
		{"missing visibility", func(in *CreateArtifactInput) { in.Access.Visibility = "" }, "internal"},
		{"zero expiry", func(in *CreateArtifactInput) { in.ExpiresAt = time.Time{} }, "internal"},
		{"nil body", func(in *CreateArtifactInput) { in.Body = nil }, "internal"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := validCreateInput()
			tc.mutate(&in)
			assertValidateResult(t, in.validate(sharetype.Default()), tc.want)
		})
	}
}

// assertValidateResult checks err against want: "" for none, "internal" for an
// internal error, else the single violation's "field/reason".
func assertValidateResult(t *testing.T, err error, want string) {
	t.Helper()
	switch want {
	case "":
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case "internal":
		if err == nil || errs.CodeOf(err) != errs.CodeInternal || errs.ViolationsOf(err) != nil {
			t.Fatalf("err = %v (code %q), want an internal error with no violations", err, errs.CodeOf(err))
		}
	default:
		if errs.CodeOf(err) != errs.CodeValidation || !errors.Is(err, errs.ErrValidation) {
			t.Fatalf("err = %v (code %q), want validation_failed", err, errs.CodeOf(err))
		}
		vs := errs.ViolationsOf(err)
		if len(vs) != 1 {
			t.Fatalf("violations = %+v, want one", vs)
		}
		if got := vs[0].Field + "/" + string(vs[0].Reason); got != want || vs[0].Location != errs.LocBody {
			t.Fatalf("violation = %+v, want %s in the body", vs[0], want)
		}
	}
}

// The unknown-type violation names every type a caller could have sent, and
// never offers bundle.
func TestCreateArtifactUnknownTypeListsAllowed(t *testing.T) {
	in := validCreateInput()
	in.ShareType = "exotic"
	v := errs.ViolationsOf(in.validate(sharetype.Default()))[0]
	limit, _ := v.Limit.(string)
	for _, want := range []string{"file", "markdown", "code", "image"} {
		if !strings.Contains(limit, want) {
			t.Fatalf("limit = %q, want it to list %q", limit, want)
		}
	}
	if strings.Contains(limit, "bundle") || v.Value == nil || *v.Value != "exotic" {
		t.Fatalf("violation = %+v (limit %q)", v, limit)
	}
}

func validBundleInput(names ...string) CreateBundleInput {
	in := CreateBundleInput{
		Provenance: artifact.Provenance{ActorID: "u1", Channel: artifact.ChannelCLI, CapturedAt: time.Unix(1, 0)},
		Access:     artifact.AccessPolicy{OwnerID: "u1", Visibility: artifact.VisibilityLink},
		ExpiresAt:  time.Unix(100, 0),
	}
	for _, n := range names {
		in.Members = append(in.Members, MemberInput{Name: n, Body: strings.NewReader("x")})
	}
	return in
}

func TestCreateBundleInputValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CreateBundleInput)
		want   string
	}{
		{"valid", func(*CreateBundleInput) {}, ""},
		{"no members", func(in *CreateBundleInput) { in.Members = nil }, "members/required"},
		{"too many members", func(in *CreateBundleInput) {
			in.Members = make([]MemberInput, MaxBundleMembers+1)
		}, "members/too_many"},
		{"empty name", func(in *CreateBundleInput) { in.Members[1].Name = "" }, "members[1].name/required"},
		{"duplicate name", func(in *CreateBundleInput) { in.Members[1].Name = "a.md" }, "members[1].name/duplicate"},
		{"title too long", func(in *CreateBundleInput) { in.Title = strings.Repeat("t", artifact.MaxTitleBytes+1) }, "title/too_long"},
		{"missing channel", func(in *CreateBundleInput) { in.Provenance.Channel = "" }, "internal"},
		{"missing actor", func(in *CreateBundleInput) { in.Provenance.ActorID = "" }, "internal"},
		{"missing owner", func(in *CreateBundleInput) { in.Access.OwnerID = "" }, "internal"},
		{"missing visibility", func(in *CreateBundleInput) { in.Access.Visibility = "" }, "internal"},
		{"zero expiry", func(in *CreateBundleInput) { in.ExpiresAt = time.Time{} }, "internal"},
		{"nil member body", func(in *CreateBundleInput) { in.Members[0].Body = nil }, "internal"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := validBundleInput("a.md", "b.md")
			tc.mutate(&in)
			assertValidateResult(t, in.validate(), tc.want)
		})
	}
}

// SPEC-0019 VE-4: every bad member name is reported, each on its own member.
func TestCreateBundleInputReportsEveryBadMember(t *testing.T) {
	in := validBundleInput("a.md", "", "a.md", "b.md", "b.md")
	vs := errs.ViolationsOf(in.validate())
	want := []string{"members[1].name/required", "members[2].name/duplicate", "members[4].name/duplicate"}
	if len(vs) != len(want) {
		t.Fatalf("violations = %+v, want %v", vs, want)
	}
	for i, v := range vs {
		if got := v.Field + "/" + string(v.Reason); got != want[i] {
			t.Fatalf("violation %d = %s, want %s", i, got, want[i])
		}
	}
	if vs[1].Value == nil || *vs[1].Value != "a.md" {
		t.Fatalf("duplicate = %+v, want the name echoed", vs[1])
	}
}
