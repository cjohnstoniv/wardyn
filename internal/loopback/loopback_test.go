// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package loopback

import "testing"

// The first rows are the secret-store copy's own test, moved here. The rest are where the copies Host replaced
// differed: case, a trailing dot, a port, brackets, and a long s that strings.EqualFold folds to "s".
func TestHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost":              true,
		"127.0.0.1":              true,
		"::1":                    true,
		"10.0.0.1":               false,
		"vault.example":          false,
		"LOCALHOST":              false,
		"":                       false,
		"127.9.9.9":              true,
		"0:0:0:0:0:0:0:1":        true,
		"::ffff:127.0.0.1":       true,
		"::FFFF:127.0.0.1":       true,
		"Localhost":              false,
		"localhost.":             false,
		" localhost":             false,
		"localhost:8080":         false,
		"127.0.0.1:8080":         false,
		"[::1]":                  false,
		"localhost.example.com":  false,
		"127.0.0.1.attacker.com": false,
		"evil-localhost.com":     false,
		"0.0.0.0":                false,
		"::":                     false,
		"localhoſt":              false,
	} {
		if got := Host(host); got != want {
			t.Errorf("Host(%q) = %v, want %v", host, got, want)
		}
	}
}
