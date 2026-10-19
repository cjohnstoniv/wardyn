// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package hostrules

import "testing"

// HostOf names the host of a URL, with or without a scheme, port, path or login.
func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://registry.corp/npm/":                   "registry.corp",
		"HTTPS://Registry.Corp:8443/x":                 "registry.corp",
		"registry.corp:443":                            "registry.corp",
		"https://user:pw@registry.corp/x":              "registry.corp",
		"https://user.name:pw@registry.corp/x":         "registry.corp",
		"user.name:pw@registry.corp":                   "registry.corp",
		"https://user.name:p.w@registry.corp:8443/a@b": "registry.corp",
		"https://registry.corp/a@evil.example":         "registry.corp",
		"https://registry.corp?next=a@evil.com":        "",
		"https://":                                     "",
		"nodots":                                       "",
	} {
		if got := HostOf(in); got != want {
			t.Errorf("HostOf(%q) = %q, want %q", in, got, want)
		}
	}
}
