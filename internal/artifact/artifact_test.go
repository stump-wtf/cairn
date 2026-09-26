package artifact

import (
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/errs"
)

func validArtifact() Artifact {
	return Artifact{
		PublicID:   "9qz1aB2c",
		ShareType:  TypeFile,
		BodySHA256: "abc123",
		Provenance: Provenance{
			ActorID:    "user_1",
			Channel:    ChannelCLI,
			CapturedAt: time.Now(),
		},
		Access:    AccessPolicy{OwnerID: "user_1", Visibility: VisibilityLink},
		ExpiresAt: time.Now().Add(time.Hour),
	}
}

func TestArtifactValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Artifact)
		wantErr bool
	}{
		{"valid", func(*Artifact) {}, false},
		{"bundle needs no body", func(a *Artifact) { a.ShareType = TypeBundle; a.BodySHA256 = "" }, false},
		{"missing public id", func(a *Artifact) { a.PublicID = "" }, true},
		{"missing share type", func(a *Artifact) { a.ShareType = "" }, true},
		{"missing body ref", func(a *Artifact) { a.BodySHA256 = "" }, true},
		{"missing channel", func(a *Artifact) { a.Provenance.Channel = "" }, true},
		{"missing actor", func(a *Artifact) { a.Provenance.ActorID = "" }, true},
		{"zero captured_at", func(a *Artifact) { a.Provenance.CapturedAt = time.Time{} }, true},
		{"missing owner", func(a *Artifact) { a.Access.OwnerID = "" }, true},
		{"missing visibility", func(a *Artifact) { a.Access.Visibility = "" }, true},
		{"zero expiry", func(a *Artifact) { a.ExpiresAt = time.Time{} }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := validArtifact()
			tt.mutate(&a)
			err := a.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected a validation error, got nil")
				}
				if errs.CodeOf(err) != errs.CodeValidation {
					t.Fatalf("code = %q, want %q", errs.CodeOf(err), errs.CodeValidation)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
