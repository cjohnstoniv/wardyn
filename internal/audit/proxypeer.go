// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"
)

type proxyPeerKey struct{}

// WithProxyPeer marks a request whose source_ip came from a trusted proxy's
// X-Forwarded-For; peer is the TCP peer that sent it.
func WithProxyPeer(ctx context.Context, peer string) context.Context {
	return context.WithValue(ctx, proxyPeerKey{}, peer)
}

// StampProxyPeer adds data.proxy_peer to a row with a source_ip when ctx
// carries one, so a row whose source_ip a proxy reported still names the
// connection Wardyn saw. Data that is not a JSON object is kept whole under
// "data".
func StampProxyPeer(ctx context.Context, sourceIP string, data json.RawMessage) json.RawMessage {
	peer, ok := ctx.Value(proxyPeerKey{}).(string)
	if !ok || peer == "" || sourceIP == "" {
		return data
	}
	fields := map[string]json.RawMessage{}
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
			fields = map[string]json.RawMessage{"data": data}
		}
	}
	fields["proxy_peer"], _ = json.Marshal(peer)
	out, _ := json.Marshal(fields)
	return out
}
