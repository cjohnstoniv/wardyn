// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package entrafake

import "github.com/google/uuid"

// objectID is the id_token's `oid`. A tenant gives each person their own, so each picker identity
// gets one derived from its subject; two people sharing one would be one Entra identity bound to two
// principals, which a sign-in refuses. Without identities the fake's one subject keeps a fixed id.
func objectID(who *Identity) string {
	if who == nil {
		return "00000000-1111-2222-3333-444444444444"
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(who.Subject)).String()
}
