// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"slices"
	"testing"
)

func TestProxyHostnames(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want []string
	}{
		{"", []string{"wardyn-proxy"}},
		{"http://wardyn-proxy:3128", []string{"wardyn-proxy"}},
		{"http://WARDYN-PROXY:3128", []string{"wardyn-proxy"}},
		{"http://proxy.corp.example:3128", []string{"wardyn-proxy", "proxy.corp.example"}},
		{"http://Proxy.Corp.Example:3128", []string{"wardyn-proxy", "proxy.corp.example"}},
		{"http://proxy.corp.example", []string{"wardyn-proxy", "proxy.corp.example"}},
		{"http://10.1.2.3:3128", []string{"wardyn-proxy"}},
		{"http://[2001:db8::1]:3128", []string{"wardyn-proxy"}},
		{"http://github.com:2001:db8::3128", []string{"wardyn-proxy"}},
		{"http://%zz", []string{"wardyn-proxy"}},
		{"not a url", []string{"wardyn-proxy"}},
	} {
		if got := ProxyHostnames(map[string]string{"WARDYN_PROXY_URL": tc.url}); !slices.Equal(got, tc.want) {
			t.Errorf("ProxyHostnames(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
	if got := ProxyHostnames(nil); !slices.Equal(got, []string{"wardyn-proxy"}) {
		t.Errorf("ProxyHostnames(nil) = %q", got)
	}
}
