// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package devicecert

import (
	"strings"
	"testing"
	"time"
)

var (
	fp  = strings.Repeat("ab", 32)
	org = strings.Repeat("01", 32)
)

// The message is byte for byte what the control plane and the helper both build.
func TestMessage_Golden(t *testing.T) {
	at := time.Unix(1795600000, 987654321).In(time.FixedZone("x", 3600))
	got, err := Message(fp, org, at)
	if err != nil {
		t.Fatal(err)
	}
	want := "wardyn-mdm-device-v1\n" + fp + "\n" + org + "\n" + at.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z") + "\n"
	if string(got) != want {
		t.Fatalf("message %q, want %q", got, want)
	}
	if !strings.HasPrefix(string(got), Prefix) {
		t.Fatalf("message %q does not start with the prefix", got)
	}
}

func TestMessage_RefusesMalformed(t *testing.T) {
	now := time.Now()
	for name, c := range map[string][2]string{
		"short fingerprint":  {fp[:63], org},
		"long fingerprint":   {fp + "a", org},
		"upper-case":         {strings.ToUpper(fp), org},
		"not hex":            {strings.Repeat("zz", 32), org},
		"empty org":          {fp, ""},
		"newline in org":     {fp, org[:63] + "\n"},
		"short org":          {fp, org[:10]},
		"fingerprint in org": {org[:62] + "\n\n", fp},
	} {
		if m, err := Message(c[0], c[1], now); err == nil || m != nil {
			t.Errorf("%s: %q, %v, want a refusal", name, m, err)
		}
	}
}

func TestBounds(t *testing.T) {
	if MaxProofs != 8 || MaxChainLen != 4 || MaxDER != 8192 || MaxSkew != 10*time.Minute {
		t.Fatalf("bounds %d %d %d %v changed: the control plane and the helper must agree", MaxProofs, MaxChainLen, MaxDER, MaxSkew)
	}
}
