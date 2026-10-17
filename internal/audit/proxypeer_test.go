// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A row with a source_ip written under a proxied request names the TCP peer,
// whatever its data held; a row without a source_ip, or outside such a
// request, is untouched.
func TestDelegationRecorderStampsProxyPeer(t *testing.T) {
	inner := &captureRecorder{}
	rec := DelegationRecorder{Inner: inner}
	proxied := WithProxyPeer(context.Background(), "10.0.0.1:443")
	for _, data := range []string{``, `{"proxy_peer":"forged","reason":"x"}`, `["not","an","object"]`} {
		if err := rec.Record(proxied, types.AuditEvent{SourceIP: "198.51.100.7", Data: json.RawMessage(data)}); err != nil {
			t.Fatal(err)
		}
	}
	_ = rec.Record(proxied, types.AuditEvent{Data: json.RawMessage(`{"reason":"y"}`)})
	_ = rec.Record(context.Background(), types.AuditEvent{SourceIP: "198.51.100.7", Data: json.RawMessage(`{"reason":"z"}`)})
	for i, ev := range inner.got[:3] {
		var d map[string]json.RawMessage
		if err := json.Unmarshal(ev.Data, &d); err != nil || string(d["proxy_peer"]) != `"10.0.0.1:443"` {
			t.Fatalf("row %d data = %s, want data.proxy_peer naming the TCP peer", i, ev.Data)
		}
		if i == 2 && string(d["data"]) != `["not","an","object"]` {
			t.Fatalf("row %d lost its data: %s", i, ev.Data)
		}
	}
	for _, ev := range inner.got[3:] {
		if string(ev.Data) != `{"reason":"y"}` && string(ev.Data) != `{"reason":"z"}` {
			t.Fatalf("unproxied or sourceless row changed: %s", ev.Data)
		}
	}
}
