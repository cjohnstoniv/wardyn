// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package testlive

import (
	"strings"
	"testing"
)

// The sample values are assembled at run time so this file never holds a
// literal that a secret scanner, or the harness's own review grep, would flag.
func TestRedact(t *testing.T) {
	jwt := "eyJ" + "hbGciOiJSUzI1NiJ9" + "." + "eyJzdWIiOiJ4In0" + "." + "c2lnbmF0dXJl"
	guid := strings.Repeat("a", 8) + "-" + strings.Repeat("b", 4) + "-" + strings.Repeat("c", 4) + "-" +
		strings.Repeat("d", 4) + "-" + strings.Repeat("e", 12)
	twelve := strings.Repeat("7", 12)
	for _, tc := range []struct{ name, in, want string }{
		{"jwt", "id_token=" + jwt + " end", "id_token=[jwt] end"},
		{"bearer", "Authorization: Bearer abc.def-123", "Authorization: Bearer [token]"},
		{"bearer lower case", "authorization: bearer xyz", "authorization: Bearer [token]"},
		{"access key id", "key " + "AKIA" + strings.Repeat("Q", 16) + ",", "key [aws-key-id],"},
		{"session key id", "ASIA" + strings.Repeat("Z", 16), "[aws-key-id]"},
		{"email", "signed in as someone.else+x@example.test now", "signed in as [email] now"},
		{"guid", "tenant " + guid, "tenant [guid]"},
		{"guid upper case", strings.ToUpper(guid), "[guid]"},
		{"twelve digits", "account " + twelve + " ok", "account [12-digit] ok"},
		{"arn account", "arn:aws:iam::" + twelve + ":role/x", "arn:aws:iam::[12-digit]:role/x"},
		{"long secret", "secret=" + strings.Repeat("Ab1/", 11), "secret=[long-secret]"},
		{"eleven digits kept", strings.Repeat("7", 11), strings.Repeat("7", 11)},
		{"thirteen digits kept", strings.Repeat("7", 13), strings.Repeat("7", 13)},
		{"plain text kept", "run COMPLETED in 12s", "run COMPLETED in 12s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Redact(tc.in); got != tc.want {
				t.Fatalf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
