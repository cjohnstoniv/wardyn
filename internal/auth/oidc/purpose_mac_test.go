// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"bytes"
	"testing"
)

func TestPurposeMAC(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	a, b := &Authenticator{hmacKey: key}, &Authenticator{hmacKey: key}
	other := &Authenticator{hmacKey: bytes.Repeat([]byte{8}, 32)}
	data := []byte("payload")
	if !bytes.Equal(a.PurposeMAC("p", data), b.PurposeMAC("p", data)) {
		t.Error("two replicas sharing a key disagree")
	}
	if bytes.Equal(a.PurposeMAC("p", data), a.PurposeMAC("q", data)) {
		t.Error("two purposes share a MAC")
	}
	if bytes.Equal(a.PurposeMAC("p", data), a.PurposeMAC("p", []byte("other"))) {
		t.Error("two payloads share a MAC")
	}
	if bytes.Equal(a.PurposeMAC("p", data), other.PurposeMAC("p", data)) {
		t.Error("two keys share a MAC")
	}
	if bytes.Equal(a.PurposeMAC("p", data), sessionHMAC(key, data)) {
		t.Error("a purpose MAC equals a session MAC")
	}
}
