package store

import (
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
)

// validInput is a minimally-complete single-body create request for exercising
// validate() in isolation (no object store or database involved).
func validCreateInput() CreateArtifactInput {
	return CreateArtifactInput{
		ShareType:  artifact.TypeFile,
		Body:       strings.NewReader("x"),
		Provenance: artifact.Provenance{CreatedByUserID: testOwner, ActorID: "u1", Channel: artifact.ChannelCLI, CapturedAt: time.Unix(1, 0)},
		Access:     artifact.AccessPolicy{OwnerUserID: testOwner, Visibility: artifact.VisibilityLink},
		ExpiresAt:  time.Unix(100, 0),
	}
}

func TestCreateArtifactInputValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*CreateArtifactInput)
		wantErr bool
	}{
		{"valid", func(*CreateArtifactInput) {}, false},
		{"missing share type", func(in *CreateArtifactInput) { in.ShareType = "" }, true},
		{"bundle rejected on single path", func(in *CreateArtifactInput) { in.ShareType = artifact.TypeBundle }, true},
		{"missing channel", func(in *CreateArtifactInput) { in.Provenance.Channel = "" }, true},
		{"missing actor", func(in *CreateArtifactInput) { in.Provenance.ActorID = "" }, true},
		{"missing owner", func(in *CreateArtifactInput) { in.Access.OwnerUserID = "" }, true},
		{"missing visibility", func(in *CreateArtifactInput) { in.Access.Visibility = "" }, true},
		{"zero expiry", func(in *CreateArtifactInput) { in.ExpiresAt = time.Time{} }, true},
		{"nil body", func(in *CreateArtifactInput) { in.Body = nil }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := validCreateInput()
			tc.mutate(&in)
			err := in.validate()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected a validation error")
				}
				if errs.CodeOf(err) != errs.CodeValidation {
					t.Fatalf("code = %q, want validation", errs.CodeOf(err))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
