// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/google/uuid"
)

func TestParseAttachArg(t *testing.T) {
	id := uuid.New()
	cases := []struct {
		in           string
		wantTarget   string
		wantWritable bool
		wantErr      bool
	}{
		{in: id.String()},
		{in: id.String() + "@/work/api", wantTarget: "/work/api"},
		{in: id.String() + ":rw", wantWritable: true},
		{in: id.String() + ":ro"},
		{in: id.String() + "@/work/api:rw", wantTarget: "/work/api", wantWritable: true},
		{in: "not-a-uuid", wantErr: true},
		{in: "not-a-uuid@/t:rw", wantErr: true},
	}
	for _, c := range cases {
		gotID, target, writable, err := parseAttachArg(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseAttachArg(%q): want error, got id=%s", c.in, gotID)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAttachArg(%q): unexpected error: %v", c.in, err)
			continue
		}
		if gotID != id || target != c.wantTarget || writable != c.wantWritable {
			t.Errorf("parseAttachArg(%q) = (%s, %q, %v), want (%s, %q, %v)",
				c.in, gotID, target, writable, id, c.wantTarget, c.wantWritable)
		}
	}
}
