// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// LiveDenyDirEnv names the directory a Kubernetes proxy keeps its live egress edit denies in:
// liveDenyFile, which the proxy applies at every start, and narrowSocket, which takes new ones.
const LiveDenyDirEnv = "WARDYN_PROXY_LIVE_DENY_DIR"

// MaxLiveDenies is the most hosts a live deny list holds.
const MaxLiveDenies = 1024

// MaxLiveDenyEntryBytes is the longest list entry: `*.`, a 253-byte DNS name and `:65535`: the longest
// entry ValidDomainEntry accepts that can match a host.
const MaxLiveDenyEntryBytes = 261

// MaxNarrowMessageBytes bounds every narrowing message: the socket's request, the client's stdin and
// answer, and the driver's stdout. The largest list is 1,024 × (261 + 3) + 1 = 270,337 bytes of JSON,
// since the entry charset needs no escaping, and an answer adds under 300 bytes.
const MaxNarrowMessageBytes = 512 << 10

const (
	liveDenyFile = "live-denies.json"
	narrowSocket = "narrow.sock"
)

// liveDenyKey is the proxy config key WithLiveDenies sets.
const liveDenyKey = "live_denies"

// NormalizeLiveDenies lower-cases and trims each host, refuses an empty, over-long or invalid entry
// and a list over MaxLiveDenies, and answers the hosts sorted and without duplicates.
func NormalizeLiveDenies(hosts []string) ([]string, error) {
	out := make([]string, 0, len(hosts))
	for i, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		switch {
		case h == "":
			return nil, fmt.Errorf("live deny entry %d is empty", i)
		case len(h) > MaxLiveDenyEntryBytes:
			return nil, fmt.Errorf("live deny entry %d is %d bytes; the longest allowed is %d", i, len(h), MaxLiveDenyEntryBytes)
		}
		if err := ValidDomainEntry(h); err != nil {
			return nil, fmt.Errorf("live deny entry %d: %w", i, err)
		}
		out = append(out, h)
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > MaxLiveDenies {
		return nil, fmt.Errorf("live deny list has %d entries; the most allowed is %d", len(out), MaxLiveDenies)
	}
	return out, nil
}

// decodeConfigMap decodes a proxy config into its top-level keys.
func decodeConfigMap(b []byte) (map[string]json.RawMessage, error) {
	var data map[string]json.RawMessage
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, errors.New("proxy config is empty")
	}
	return data, nil
}

// WithLiveDenies returns the proxy config b with hosts added to its live deny list. With nothing to add
// and no list in b it answers b itself. Otherwise the output is the keys of b, sorted, with the list
// the sorted union of the one b carried and hosts: the encoding runner.WithProxyConfigKeys gives.
func WithLiveDenies(b []byte, hosts []string) ([]byte, error) {
	data, err := decodeConfigMap(b)
	if err != nil {
		return nil, err
	}
	have, hasKey := data[liveDenyKey]
	var existing []string
	if hasKey {
		if err := json.Unmarshal(have, &existing); err != nil {
			return nil, fmt.Errorf("live_denies: %w", err)
		}
	}
	union, err := NormalizeLiveDenies(append(existing, hosts...))
	if err != nil {
		return nil, err
	}
	if len(union) == 0 && !hasKey {
		return b, nil
	}
	raw, err := json.Marshal(union)
	if err != nil {
		return nil, err
	}
	data[liveDenyKey] = raw
	return json.Marshal(data)
}

// LiveDenyBaseSHA256 is ConfigSHA256 of the canonical encoding of b without its live deny list: a key
// to compare two configs by everything but that list, never a hash a proxy reports.
func LiveDenyBaseSHA256(b []byte) (string, error) {
	data, err := decodeConfigMap(b)
	if err != nil {
		return "", err
	}
	delete(data, liveDenyKey)
	out, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	return ConfigSHA256(out), nil
}

// liveDenyList is the live deny list the config bytes b carry, nil when they carry none.
func liveDenyList(b []byte) ([]string, error) {
	data, err := decodeConfigMap(b)
	if err != nil {
		return nil, err
	}
	var list []string
	if raw, ok := data[liveDenyKey]; ok {
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("live_denies: %w", err)
		}
	}
	return list, nil
}

// liveDenies holds the deny-only policy a live egress edit swaps in. Its pointer is shared by every copy
// of the Policy that owns it.
type liveDenies struct{ p atomic.Pointer[Policy] }

// denies reports whether the live list denies host on port, by the same matching a policy's denied
// domains use. A nil receiver denies nothing.
func (l *liveDenies) denies(host string, port int) bool {
	if l == nil {
		return false
	}
	p := l.p.Load()
	return p != nil && p.evalHost(host, port) == hostDeny
}

// set replaces the list. The policy it compiles has no live list of its own, so matching never recurses.
func (l *liveDenies) set(hosts []string) {
	if len(hosts) == 0 {
		l.p.Store(nil)
		return
	}
	l.p.Store(CompilePolicy(types.RunPolicySpec{DeniedDomains: hosts}))
}
