// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"strconv"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run with a correlation ID sends each approve_each call at most once per occurrence within that
// ID (MCPApproveEachConfig.SendOnce). A call without its caller's own operation key is given one made
// from the call itself: its hash (types.MCPCallKey: the server, the raw tool name, the canonical
// arguments and extra params) and which occurrence of that call it is in this proxy, counting each
// one recorded as sent or answered as sent. The control plane keeps one record per key in the run's
// namespace (mcp_operation_key.go), so:
//   - a retry in a new run of the same correlation ID has a new proxy, whose first such call is the
//     first occurrence again: it is answered with the earlier send, not asked about or sent again;
//   - within one proxy the same call made again after its send is the next occurrence, asked about
//     and sent as before;
//   - a call with other arguments, or with its caller's own key, is an operation of its own.
//
// The permission tool's gate raises the call under the same key (gateScope), so the broker spends
// the gate's approval. A caller's own key may not start with derivedKeyPrefix, with send once or
// without it (readCall): another call sent under such a key would hold the key a call is given, and
// that call could not be sent in the namespace while the record answers for the key.

// derivedKeyPrefix begins an operation key made from a call: the prefix, the 64-character hash, a
// colon and the occurrence, well within types.MaxOperationKeyLen.
const derivedKeyPrefix = types.DerivedOperationKeyPrefix

// mcpCallOperationKeyMadeText is what the agent reads for a call whose own operation key starts with
// derivedKeyPrefix.
const mcpCallOperationKeyMadeText = "This call's operation key (_meta \"wardyn.dev/operation-key\") starts with \"call:\", which is reserved for the keys Wardyn makes from calls, so Wardyn did not ask about it or send it."

// operationKey is the key a call is raised and sent under, and the occurrence it counts as (0 for
// none): its caller's own key when it has one; else, when the run sends each call once, the key made
// from callSHA256 and its next occurrence; else none.
func (b *mcpBroker) operationKey(own, callSHA256 string) (string, int) {
	if own != "" || b == nil || !b.sendOnce {
		return own, 0
	}
	n := b.calls.occurrence(callSHA256)
	return derivedKeyPrefix + callSHA256 + ":" + strconv.Itoa(n), n
}

// occurrence is which occurrence of the call with hash sha the next one is: one past the last one
// done (occurred).
func (c *mcpCalls) occurrence(sha string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.done[sha] + 1
}

// occurred counts occurrence n of the call with hash sha as done, once it was recorded as sent or
// answered as sent, so the next call with that hash is occurrence n+1. Counting one twice, or an
// earlier one late, changes nothing; n 0 (no key made from the call) is ignored.
func (c *mcpCalls) occurred(sha string, n int) {
	if n == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done == nil {
		c.done = map[string]int{}
	}
	c.done[sha] = max(c.done[sha], n)
}
