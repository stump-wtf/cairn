// Package operator is Cairn's operator profile (ADR-0029, SPEC-0023 REQ
// "Operator and User Profiles"): who runs the instance, and the small,
// audited set of things they may do to it.
//
// The operator is configuration, not a role stored on a user. A signed-in
// user is an operator while their session's recorded provenance, written
// "<issuer>|<subject>", is listed in CAIRN_OPERATORS, or while
// CAIRN_OPERATOR_GROUP is set and the OIDC groups claim their session was
// minted from carried it. With neither variable set the instance has no
// operator and every operator route answers 404. An operator is also an
// ordinary user: they own their personal artifacts exactly as anyone does,
// and operator status never widens what they may read.
//
// Operator surfaces bound tenant data and never read it (SPEC-0023 REQ
// "Operator Surfaces Bound Tenant Data and Never Read It"): the directory
// here returns counts, never an artifact's body, title, link, tags or
// annotations. Every action that affects a tenant writes an operator_audit
// row in the same transaction as the action itself.
//
// Governing: ADR-0029, SPEC-0023 REQ "Operator and User Profiles", REQ
// "Operator Surfaces Bound Tenant Data and Never Read It".
package operator

import (
	"fmt"
	"strings"
)

// Identity is one CAIRN_OPERATORS entry: the (issuer, subject) a sign-in
// records on its session, exactly as it records them (the OIDC issuer is the
// configured CAIRN_OIDC_ISSUER value; a GitHub sign-in records GitHub's
// origin and the numeric account id).
type Identity struct {
	Issuer  string
	Subject string
}

// String renders the identity in its configuration form, "<issuer>|<subject>".
func (i Identity) String() string { return i.Issuer + "|" + i.Subject }

// Set is the configured operator profile. The zero value and nil both mean
// "no operator": Enabled reports false and nothing matches.
type Set struct {
	ids   map[Identity]struct{}
	group string
}

// Parse builds the operator profile from CAIRN_OPERATORS (comma-separated
// "<issuer>|<subject>" entries) and CAIRN_OPERATOR_GROUP. An entry splits at
// its FIRST "|", because issuers are URLs and never contain one while
// subjects may ("auth0|123"). A malformed entry is a boot error naming its
// 1-based position, never a silently dropped operator.
func Parse(operators, group string) (*Set, error) {
	s := &Set{ids: map[Identity]struct{}{}, group: strings.TrimSpace(group)}
	n := 0
	for _, entry := range strings.Split(operators, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		n++
		issuer, subject, ok := strings.Cut(entry, "|")
		issuer, subject = strings.TrimSpace(issuer), strings.TrimSpace(subject)
		if !ok || issuer == "" || subject == "" {
			return nil, fmt.Errorf("CAIRN_OPERATORS entry %d: want <issuer>|<subject>", n)
		}
		s.ids[Identity{Issuer: issuer, Subject: subject}] = struct{}{}
	}
	return s, nil
}

// Enabled reports whether the instance has an operator at all. When it is
// false every operator route answers 404 (SPEC-0023 "No operator
// configured").
func (s *Set) Enabled() bool {
	return s != nil && (len(s.ids) > 0 || s.group != "")
}

// Group is the configured CAIRN_OPERATOR_GROUP, or "".
func (s *Set) Group() string {
	if s == nil {
		return ""
	}
	return s.group
}

// Len is the number of distinct CAIRN_OPERATORS identities.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.ids)
}

// Lists reports whether (issuer, subject) is a CAIRN_OPERATORS identity. An
// empty issuer or subject (the development login records neither) never
// matches.
func (s *Set) Lists(issuer, subject string) bool {
	if s == nil || issuer == "" || subject == "" {
		return false
	}
	_, ok := s.ids[Identity{Issuer: issuer, Subject: subject}]
	return ok
}

// IsOperator reports whether a session is an operator's: its provenance is
// listed, or it recorded the configured operator group at sign-in. A session
// that recorded a group which is no longer the configured one is not an
// operator's, so renaming or unsetting CAIRN_OPERATOR_GROUP demotes it.
func (s *Set) IsOperator(issuer, subject, sessionGroup string) bool {
	if s.Lists(issuer, subject) {
		return true
	}
	return s.Group() != "" && sessionGroup == s.Group()
}

// identities returns the listed identities as parallel issuer and subject
// slices, for a parameterized unnest() match against user_identities.
func (s *Set) identities() (issuers, subjects []string) {
	if s == nil {
		return nil, nil
	}
	for id := range s.ids {
		issuers = append(issuers, id.Issuer)
		subjects = append(subjects, id.Subject)
	}
	return issuers, subjects
}
