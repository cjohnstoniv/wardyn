// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"net/url"
	"strings"
)

// PortalPersonPrefix starts the principal of every person an issuer portal introduces:
// delegate:person:<issuer key>:<person id>. It is reserved at every human door.
const PortalPersonPrefix = "delegate:person:"

// ErrDelegatePersonNotAllowed refuses a person list that confers an admin, sponsor, approver or console role
// on a principal that names a portal person. A portal person never holds one.
var ErrDelegatePersonNotAllowed = errors.New("types: a portal person may not hold this role")

// IsPortalPersonPrincipal reports whether principal names a portal person, after the same fold the user-grant
// path applies.
func IsPortalPersonPrincipal(principal string) bool {
	return strings.HasPrefix(CanonicalUserSubject(principal), PortalPersonPrefix)
}

// RefuseDelegatePersonSubject is the prefix-only half of the ruling on admin-written person lists: it
// returns ErrDelegatePersonNotAllowed for a user subject that names a portal person, and nil for anything
// else. It reads nothing, so a package outside the store can call it. Lists that may name a portal person
// are ruled in the store, against the issuer bindings.
func RefuseDelegatePersonSubject(subjectType, subject string) error {
	if subjectType == "user" && IsPortalPersonPrincipal(subject) {
		return ErrDelegatePersonNotAllowed
	}
	return nil
}

// PortalIssuerKey is the key k of an issuer: the first 26 characters of the lowercase, unpadded base32 of
// SHA-256 over "wardyn-portal-v2\n" + issuer. issuer is the exact registered bytes, never normalised, so two
// spellings of one issuer are two keys, and so two sets of people.
func PortalIssuerKey(issuer string) string {
	sum := sha256.Sum256([]byte("wardyn-portal-v2\n" + issuer))
	return strings.ToLower(portalBase32.EncodeToString(sum[:]))[:26]
}

// PortalPrincipal is the principal of the person with the given id under the issuer key k:
// delegate:person:<k>:<e>, where e is the lowercase, unpadded base32 of the exact person id bytes. Lowercase
// base32 is unchanged by lowercasing, so two ids that differ only in case are two principals under every
// comparison the user-grant path makes.
func PortalPrincipal(issuerKey, personID string) string {
	return PortalPersonPrefix + issuerKey + ":" + strings.ToLower(portalBase32.EncodeToString([]byte(personID)))
}

var portalBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NormIssuerURL is norm(u): the scheme and host lowercased, a ":443" port dropped and one trailing "/" of the
// path stripped; "" stays "". It applies to the stored internal address of a binding, to the check that two
// spellings of one issuer are not both bound, and to the console-issuer rule, where a broad match is the safe
// direction. It is never applied to the issuer that is hashed into a person's key.
func NormIssuerURL(u string) string {
	if u == "" {
		return ""
	}
	p, err := url.Parse(u)
	if err != nil || p.Scheme == "" || p.Host == "" {
		return u
	}
	p.Scheme = strings.ToLower(p.Scheme)
	p.Host = strings.TrimSuffix(strings.ToLower(p.Host), ":443")
	p.Path = strings.TrimSuffix(p.Path, "/")
	p.RawPath = strings.TrimSuffix(p.RawPath, "/")
	return p.String()
}

// ClientSession is one other client of the database, as the activation sample sees it: its application_name
// and its client address. Label is empty for a session that sets none or whose label the caller cannot read.
type ClientSession struct {
	Label      string `json:"label"`
	ClientAddr string `json:"client_addr"`
}
