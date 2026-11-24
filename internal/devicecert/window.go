// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package devicecert

import (
	"bytes"
	"crypto/x509"
	"time"
)

// OutsideWindow names the first certificate of a presented chain that is outside its validity window at at: the
// device's certificate, then each presented intermediate, then each configured CA that could have issued one of
// them (its subject is the issuer of the device's certificate or of an intermediate). notYetValid says the
// window had not started, else it had ended. It returns nil when every one is inside its window.
//
// Go's chain verification reports a CA that is outside its window only as an unknown authority, so a refusal
// that must name the certificate at fault has to look at the windows itself.
func OutsideWindow(at time.Time, leaf *x509.Certificate, intermediates, roots []*x509.Certificate) (bad *x509.Certificate, notYetValid bool) {
	outside := func(c *x509.Certificate) bool { return at.Before(c.NotBefore) || at.After(c.NotAfter) }
	issuers := [][]byte{leaf.RawIssuer}
	for _, c := range intermediates {
		issuers = append(issuers, c.RawIssuer)
	}
	candidates := []*x509.Certificate{leaf}
	candidates = append(candidates, intermediates...)
	for _, r := range roots {
		for _, iss := range issuers {
			if bytes.Equal(r.RawSubject, iss) {
				candidates = append(candidates, r)
				break
			}
		}
	}
	for _, c := range candidates {
		if outside(c) {
			return c, at.Before(c.NotBefore)
		}
	}
	return nil, false
}
