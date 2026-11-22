// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"encoding/json"
	"strings"
	"testing"
)

// The largest list an edit can send, 1,024 hosts of 261 bytes, fits one control frame as narrow_proxy's args.
func TestNarrowProxyArgsLargestListFitsAFrame(t *testing.T) {
	host := strings.Repeat("a", 261)
	args := NarrowProxyArgs{Ref: "runner:11111111-2222-3333-4444-555555555555/wardyn-agent-x", Deny: make([]string, 1024), PolicyGeneration: 9}
	for i := range args.Deny {
		args.Deny[i] = host
	}
	payload, err := Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	f := Frame{Type: TypeCall, Seq: 1, Stream: 2, Payload: payload}
	if _, err := f.Encode(nil); err != nil {
		t.Fatalf("a %d-byte narrow_proxy CALL does not encode: %v", len(payload), err)
	}
	if len(payload) >= MaxControlFrame {
		t.Fatalf("a %d-byte narrow_proxy payload is not under the %d control frame", len(payload), MaxControlFrame)
	}
}

// BaseChecked and the list cross the wire, and an older org's decode of caps without the flag reads it as absent.
func TestNarrowProxyResultAndCapsRoundTrip(t *testing.T) {
	in := NarrowProxyResult{ConfigSHA256: "c", BaseSHA256: "b", LiveDenies: []string{"a.example"}, Changed: true, BaseChecked: true}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out NarrowProxyResult
	if err := Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.ConfigSHA256 != "c" || out.BaseSHA256 != "b" || len(out.LiveDenies) != 1 || !out.Changed || !out.BaseChecked {
		t.Errorf("round trip = %+v; want %+v", out, in)
	}
	var caps Caps
	if err := Unmarshal([]byte(`{"proxy_narrow":true}`), &caps); err != nil || !caps.ProxyNarrow {
		t.Errorf("caps with proxy_narrow = %+v, %v; want ProxyNarrow", caps, err)
	}
	caps = Caps{}
	if err := Unmarshal([]byte(`{"proxy_fence":true}`), &caps); err != nil || caps.ProxyNarrow {
		t.Errorf("caps without proxy_narrow = %+v, %v; want ProxyNarrow off", caps, err)
	}
}
