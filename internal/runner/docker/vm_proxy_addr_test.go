// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"reflect"
	"testing"
)

func TestGuestProxyAddr(t *testing.T) {
	for _, c := range []struct {
		name, url, want string
		refused         bool
	}{
		{"unset is the default name", "", "wardyn-proxy:3128", false},
		{"the default URL is the default name", "http://wardyn-proxy:3128", "wardyn-proxy:3128", false},
		{"an override is followed", "http://relay-proxy.internal:8080", "relay-proxy.internal:8080", false},
		{"a trailing slash is fine", "http://relay-proxy:3128/", "relay-proxy:3128", false},
		{"an IPv6 literal keeps its brackets", "http://[fd00::5]:3128", "[fd00::5]:3128", false},
		{"https is refused", "https://relay-proxy:3128", "", true},
		{"no port is refused", "http://relay-proxy", "", true},
		{"a path is refused", "http://relay-proxy:3128/x", "", true},
		{"userinfo is refused", "http://u:p@relay-proxy:3128", "", true},
		{"no host is refused", "http://:3128", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := guestProxyAddr(map[string]string{"WARDYN_PROXY_URL": c.url})
			if c.refused {
				if err == nil {
					t.Fatalf("guestProxyAddr(%q) = %q, want a refusal", c.url, got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("guestProxyAddr(%q) = %q, %v, want %q", c.url, got, err, c.want)
			}
		})
	}
	if got, err := guestProxyAddr(nil); err != nil || got != "wardyn-proxy:3128" {
		t.Fatalf("guestProxyAddr(nil) = %q, %v, want the default", got, err)
	}
}

func TestRelayProxyHosts(t *testing.T) {
	for _, c := range []struct {
		guestProxy string
		want       []string
	}{
		{"wardyn-proxy:3128", []string{"wardyn-proxy:10.0.0.2"}},
		{"relay-proxy:8080", []string{"wardyn-proxy:10.0.0.2", "relay-proxy:10.0.0.2"}},
		{"10.9.9.9:3128", []string{"wardyn-proxy:10.0.0.2"}},
	} {
		if got := relayProxyHosts(c.guestProxy, "10.0.0.2"); !reflect.DeepEqual(got, c.want) {
			t.Errorf("relayProxyHosts(%q) = %q, want %q", c.guestProxy, got, c.want)
		}
	}
}
