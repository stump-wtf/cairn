package webhook

import (
	"time"

	"github.com/stump-wtf/cairn/internal/artifact"
)

// zeroTime is the explicit zero value, named for readability at test call
// sites (`in.ExpiresAt = zeroTime` reads better than a bare time.Time{}).
var zeroTime time.Time

// validProvenance / validAccess / future are the minimal valid fixtures shared
// by the unit and integration tests in this package.
func validProvenance() artifact.Provenance {
	return artifact.Provenance{
		CreatedByUserID: joeUser,
		ActorID:         "joe",
		Channel:         artifact.ChannelAPI,
		CapturedAt:      time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
	}
}

// joeUser and ownerAUser are user ids the integration harness seeds.
const (
	joeUser    = "00000000-0000-4000-8000-0000000000c1"
	ownerAUser = "00000000-0000-4000-8000-0000000000c2"
)

func validAccess() artifact.AccessPolicy {
	return artifact.AccessPolicy{OwnerUserID: joeUser, Visibility: artifact.VisibilityLink}
}

// future is a comfortably-future expiry so an endpoint resolves under the
// link-cap read policy (expires_at > now()) regardless of when the suite runs.
func future() time.Time { return time.Now().Add(30 * 24 * time.Hour) }
