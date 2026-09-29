package httpapi

import (
	"strings"
	"testing"
)

// Existing users signing in through OIDC land on their existing account and
// Bin (SPEC-0023 REQ "Users and Identities"). Before users existed an OIDC
// session acted as its email claim, so an existing user's Bin is owned by
// that email. These tests pin the three ways a sign-in reaches, or must never
// reach, that Bin.

// An issuer the operator declared authoritative for emails
// (CAIRN_OIDC_TRUST_EMAIL) reaches the Bin on the FIRST sign-in, even though
// its ID token carries no email_verified claim or a false one, as live Pocket
// ID's does for LDAP-synced users. Without the knob that sign-in was keyed on
// its subject and the Bin was unreachable.
func TestIntegrationTrustedIssuerEmailReachesExistingBin(t *testing.T) {
	for name, verified := range map[string]any{"claim absent": omitClaim{}, "claim false": false} {
		t.Run(name, func(t *testing.T) {
			idp := newFakeIdP(t)
			idp.setIdentity("pocket-joe", "Joe@Example.com", verified)
			srv := identityServerWith(t, idp, nil, func(c *Config) { c.OIDCTrustEmail = true })
			id := seedArtifact(t, srv, "joe@example.com")

			client := signInOIDC(t, srv, idp)
			if actor := whoamiActor(t, srv, client); actor != "joe@example.com" {
				t.Fatalf("actor = %q, want the trusted issuer's email", actor)
			}
			if !sessionBinIDs(t, srv, client)[id] {
				t.Fatal("the existing Bin is not reachable from the trusted issuer's first sign-in")
			}
		})
	}
}

// Matching is not one-shot: an identity whose first sign-in arrived without a
// verified email reaches the Bin on the first later sign-in that has one.
// Against a one-shot resolver the second sign-in kept the subject-keyed user
// from the first and the Bin stayed stranded.
func TestIntegrationLaterVerifiedSignInReachesExistingBin(t *testing.T) {
	idp := newFakeIdP(t)
	idp.setIdentity("pocket-joe", "joe@example.com", false)
	srv := identityServer(t, idp, nil)
	id := seedArtifact(t, srv, "joe@example.com")

	first := signInOIDC(t, srv, idp)
	if actor := whoamiActor(t, srv, first); actor != "pocket-joe" {
		t.Fatalf("unverified first sign-in acted as %q, want its subject", actor)
	}

	idp.setIdentity("pocket-joe", "joe@example.com", true)
	later := signInOIDC(t, srv, idp)
	if actor := whoamiActor(t, srv, later); actor != "joe@example.com" {
		t.Fatalf("verified second sign-in acted as %q, want joe@example.com", actor)
	}
	if !sessionBinIDs(t, srv, later)[id] {
		t.Fatal("the verified second sign-in did not reach the existing Bin")
	}
}

// The knob is scoped to the issuer the operator trusts: without it, an
// unverified email never reaches that email's Bin, however many times the
// identity signs in.
func TestIntegrationUntrustedUnverifiedEmailNeverReachesBin(t *testing.T) {
	idp := newFakeIdP(t)
	idp.setIdentity("attacker-sub", "victim@example.com", omitClaim{})
	srv := identityServer(t, idp, nil)
	id := seedArtifact(t, srv, "victim@example.com")

	for i := 0; i < 2; i++ {
		client := signInOIDC(t, srv, idp)
		actor := whoamiActor(t, srv, client)
		if strings.Contains(actor, "victim@example.com") {
			t.Fatalf("sign-in %d acted as %q", i+1, actor)
		}
		if sessionBinIDs(t, srv, client)[id] {
			t.Fatalf("sign-in %d reached the victim's Bin", i+1)
		}
	}
}
