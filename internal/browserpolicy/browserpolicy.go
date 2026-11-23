// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package browserpolicy renders the Chromium managed policy a deployment's browser lockdown delivers
// into every sandbox (#2970): Wardyn's baseline, which no organisation file can loosen, plus the
// organisation's own Chromium policies. Pure: no I/O.
package browserpolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// MaxOrgBytes caps an organisation's policy file. Every managed file of a run shares one ceiling
// (runner.ManagedFilesMaxBytes), so this one may not take most of it.
const MaxOrgBytes = 64 << 10

// baseline is what every locked-down browser runs with: no extension but those the organisation
// allows, and no developer tools.
func baseline() map[string]any {
	return map[string]any{
		"DeveloperToolsAvailability": 2,
		"ExtensionInstallBlocklist":  []string{"*"},
	}
}

// Owned are the keys an organisation's file may not set: the baseline's, the ones that override the
// extension blocklist or turn remote debugging back on, and the ones that would move the browser off
// the run's proxy or the sign-in lane.
var Owned = []string{
	"CommandLineFlagSecurityWarningsEnabled", "DeveloperToolsAvailability", "ExtensionInstallBlocklist",
	"ExtensionSettings", "ProxyBypassList", "ProxyMode", "ProxyPacUrl", "ProxyServer", "ProxyServerMode",
	"ProxySettings", "RemoteDebuggingAllowed",
}

var extensionID = regexp.MustCompile(`^[a-p]{32}$`)

var errNotObject = errors.New("the browser policy file must be one JSON object of Chromium policy names")

// Render returns the managed policy file for org, an organisation's JSON object of Chromium policies
// (empty: the baseline alone). It refuses anything but one JSON object, a file over MaxOrgBytes, an
// Owned key, and an ExtensionInstallAllowlist or ExtensionInstallForcelist entry that names no
// extension id.
func Render(org []byte) ([]byte, error) {
	out := baseline()
	if len(bytes.TrimSpace(org)) == 0 {
		return json.Marshal(out)
	}
	if len(org) > MaxOrgBytes {
		return nil, fmt.Errorf("the browser policy file is over %d bytes", MaxOrgBytes)
	}
	var in map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(org))
	if err := dec.Decode(&in); err != nil || in == nil {
		return nil, errNotObject
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, errNotObject
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// Every Proxy key is Wardyn's, named or not, so a proxy key a later Chromium adds is refused too.
		if slices.Contains(Owned, k) || strings.HasPrefix(k, "Proxy") {
			return nil, fmt.Errorf("the browser policy file sets %s, which Wardyn sets itself; remove it", k)
		}
		if k == "ExtensionInstallAllowlist" || k == "ExtensionInstallForcelist" {
			var ids []string
			if err := json.Unmarshal(in[k], &ids); err != nil {
				return nil, fmt.Errorf("the browser policy file's %s must be a list of extension ids", k)
			}
			for _, id := range ids {
				if !extensionID.MatchString(strings.SplitN(id, ";", 2)[0]) {
					return nil, fmt.Errorf("the browser policy file's %s entry %q is not an extension id (32 letters a to p)", k, id)
				}
			}
		}
		out[k] = in[k]
	}
	return json.Marshal(out)
}
