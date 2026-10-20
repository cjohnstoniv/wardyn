// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// carriedProxyVars are the proxy variables Go's HTTP clients read, in the order setup writes them.
// Setup copies the ones set in the shell into the runner service, with no user name or password.
var carriedProxyVars = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy"}

// carriedProxyValue is what a unit file, the environment file and a plist can all carry as written.
var carriedProxyValue = regexp.MustCompile(`^[A-Za-z0-9._:/%+=,* ~\[\]-]+$`)

type carriedProxy struct{ name, value string }

// carryProxy reads the shell's proxy variables for the runner service. A proxy URL comes back without
// its user name and password: stripped lists the variables that held one. Wardyn stores no proxy
// credential, and a plist is readable by every local account. A value setup cannot write is refused,
// naming the variable and never the value, since the value may hold a credential.
func carryProxy(getenv func(string) string) (carried []carriedProxy, stripped []string, err error) {
	for _, name := range carriedProxyVars {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			continue
		}
		if !strings.HasSuffix(strings.ToUpper(name), "NO_PROXY") {
			var had bool
			if v, had = stripUserinfo(v); had {
				stripped = append(stripped, name)
			}
			if !validProxyURL(v) {
				return nil, nil, fmt.Errorf("%s is not a proxy address setup can carry into the service (its value is not repeated here: it may hold a credential)", name)
			}
		}
		if !carriedProxyValue.MatchString(v) {
			return nil, nil, fmt.Errorf("%s holds characters setup cannot write into the service (its value is not repeated here: it may hold a credential)", name)
		}
		carried = append(carried, carriedProxy{name, v})
	}
	return carried, stripped, nil
}

// stripUserinfo drops everything between the scheme and the last '@': that is the user name and
// password, however they were escaped. It works on the text because a malformed URL fails to parse
// and the parser's error would repeat the password.
func stripUserinfo(v string) (string, bool) {
	scheme, rest := "", v
	if i := strings.Index(v, "://"); i >= 0 {
		scheme, rest = v[:i+3], v[i+3:]
	}
	i := strings.LastIndex(rest, "@")
	if i < 0 {
		return v, false
	}
	return scheme + rest[i+1:], true
}

// validProxyURL is whether v, with or without a scheme, names a host.
func validProxyURL(v string) bool {
	if !strings.Contains(v, "://") {
		v = "http://" + v
	}
	u, err := url.Parse(v)
	return err == nil && u.Hostname() != ""
}

// proxyCarryLine is the one line setup prints about what it carried, naming variables only.
func proxyCarryLine(carried []carriedProxy, stripped []string) string {
	names := make([]string, len(carried))
	for i, c := range carried {
		names[i] = c.name
	}
	line := "The runner service now carries " + strings.Join(names, ", ") + " from this shell (the runner service only: runs and their proxies never get them)."
	if len(stripped) > 0 {
		line += " The runner service connects through " + strings.Join(stripped, ", ") + " without the user name and password it held; a proxy that needs them will refuse it (wardyn doctor shows it). Wardyn does not store proxy passwords."
	}
	return line
}

// proxyKeptLine is the one line setup prints when the shell sets no proxy variable and the service keeps the
// ones an earlier setup carried, naming variables only.
func proxyKeptLine(carried []carriedProxy) string {
	names := make([]string, len(carried))
	for i, c := range carried {
		names[i] = c.name
	}
	return "This shell sets no proxy variable, so the runner service keeps " + strings.Join(names, ", ") + " from the earlier setup."
}
