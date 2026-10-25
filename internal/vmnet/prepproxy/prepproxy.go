// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package prepproxy renders the wardyn-proxy configuration for stage 2 of a
// macOS base's preparation ("Installing developer tools"). That proxy is the
// guest's only network: it allows Apple's software-update hosts on port 443 and
// nothing else, holds no credential, and terminates no TLS. The tunnel is opaque:
// the relay checks the SNI or HTTP Host of a flow's first flight, and the proxy
// does not assert TLS inside it, so what is enforced is port 443 only.
//
// It is the same wardyn-proxy a run uses, configured the way a run's is (JSON on
// the sidecar's stdin, WARDYN_PROXY_CONFIG_STDIN=1); this package only builds
// that JSON. The caller starts the sidecar.
package prepproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Port is the only port the prep proxy reaches on an allowed host.
const Port = 443

// controlPlaneURL is where the proxy would post its decisions and renew its run
// token. Stage 2 has no control plane, so this is a loopback address nothing
// serves: those calls fail quietly, and every decision is still printed to the
// proxy's stdout, which is where the prep records what it allowed and refused.
const controlPlaneURL = "http://127.0.0.1:9"

// noToken stands in for the run token the config requires. It authenticates to
// nothing: the control plane above is not there.
const noToken = "macos-prep-no-control-plane"

type allowlistFile struct {
	Version int      `json:"version"`
	Hosts   []string `json:"hosts"`
}

// ParseAllowlist reads deploy/macos-guest/prep-allowlist.json: a version and bare
// lower-case hostnames, with no wildcard, port, scheme or duplicate.
func ParseAllowlist(raw []byte) ([]string, error) {
	var f allowlistFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("prepproxy: allowlist: %w", err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("prepproxy: allowlist: unknown format %d", f.Version)
	}
	if len(f.Hosts) == 0 {
		return nil, fmt.Errorf("prepproxy: allowlist: no hosts")
	}
	seen := map[string]bool{}
	for _, h := range f.Hosts {
		switch {
		case h != strings.ToLower(strings.TrimSpace(h)) || strings.ContainsAny(h, "*:/@ "):
			return nil, fmt.Errorf("prepproxy: allowlist: %q is not a bare lower-case hostname", h)
		case !strings.Contains(h, ".") || strings.HasSuffix(h, "."):
			return nil, fmt.Errorf("prepproxy: allowlist: %q is not a hostname", h)
		case proxy.ValidDomainEntry(h) != nil:
			return nil, fmt.Errorf("prepproxy: allowlist: %q: %w", h, proxy.ValidDomainEntry(h))
		case seen[h]:
			return nil, fmt.Errorf("prepproxy: allowlist: %q is listed twice", h)
		}
		seen[h] = true
	}
	return f.Hosts, nil
}

// Config is the wardyn-proxy config for the stage-2 prep proxy: allowlistJSON is
// the contents of prep-allowlist.json, port the proxy's listen port, and peers
// the networks it serves (required: the runner passes the prep network's
// subnet, and an empty list is refused rather than serving every interface). Each host is allowed on port 443 only (an entry written
// host:443), every other host and port is refused, and the config carries no
// injection rule, grant or MITM key. vm is set because the guest's only client
// is wardyn-vmnet. The result is loaded through the proxy's own strict loader
// before it is returned.
func Config(allowlistJSON []byte, port int, peers []netip.Prefix) ([]byte, error) {
	if len(peers) == 0 {
		return nil, errors.New("prepproxy: the prep network's peers are required")
	}
	hosts, err := ParseAllowlist(allowlistJSON)
	if err != nil {
		return nil, err
	}
	allowed := make([]string, len(hosts))
	for i, h := range hosts {
		allowed[i] = h + ":" + strconv.Itoa(Port)
	}
	out, err := json.Marshal(proxy.Config{
		RunID:           uuid.New(),
		ControlPlaneURL: controlPlaneURL,
		RunToken:        noToken,
		Policy: types.RunPolicySpec{
			AllowedDomains:   allowed,
			FirstUseApproval: types.FirstUseAlwaysDeny,
		},
		Listen:       ":" + strconv.Itoa(port),
		AllowedPeers: peers,
		VM:           true,
	})
	if err != nil {
		return nil, fmt.Errorf("prepproxy: %w", err)
	}
	if _, err := proxy.LoadConfigBytes(out); err != nil {
		return nil, fmt.Errorf("prepproxy: %w", err)
	}
	return out, nil
}
