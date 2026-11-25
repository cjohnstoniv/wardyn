// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistant

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestDigestsChangeWithOneByte(t *testing.T) {
	secs := []types.AssistantSection{{Label: "A", Text: "one"}}
	other := []types.AssistantSection{{Label: "A", Text: "onf"}}
	untrusted := []types.AssistantSection{{Label: "A", Text: "one", Untrusted: true}}
	if RecordsDigest(secs) == RecordsDigest(other) || RecordsDigest(secs) == RecordsDigest(untrusted) {
		t.Error("RecordsDigest ignored a change")
	}
	if PromptDigest("a") == PromptDigest("b") || OutputDigest("a", []byte("x")) == OutputDigest("a", []byte("y")) ||
		OutputDigest("a", nil) == OutputDigest("b", nil) {
		t.Error("a prompt or output digest ignored a change")
	}
	if len(RecordsDigest(secs)) != 64 {
		t.Errorf("digest is %d hex chars, want 64", len(RecordsDigest(secs)))
	}
}

func TestPreviewDigestCoversTheBinding(t *testing.T) {
	secs := []types.AssistantSection{{Label: "A", Text: "one"}}
	b := types.AssistantBinding{ProviderID: "p", ProviderUID: "u", Model: "m"}
	base := PreviewDigest(b, secs)
	for name, alt := range map[string]types.AssistantBinding{
		"provider": {ProviderID: "q", ProviderUID: "u", Model: "m"},
		"uid":      {ProviderID: "p", ProviderUID: "v", Model: "m"},
		"model":    {ProviderID: "p", ProviderUID: "u", Model: "n"},
	} {
		if PreviewDigest(alt, secs) == base {
			t.Errorf("a different %s left the digest unchanged", name)
		}
	}
	if PreviewDigest(b, secs) != base {
		t.Error("the digest is not stable")
	}
	if base == RecordsDigest(secs) {
		t.Error("the preview digest equals the records digest")
	}
}

// OutputHash fed piece by piece equals OutputDigest of the whole text.
func TestOutputHashEqualsOutputDigest(t *testing.T) {
	h := NewOutputHash()
	for _, p := range []string{"one ", "two ", "three"} {
		h.Text(p)
	}
	if got, want := h.Sum([]byte(`{"x":1}`)), OutputDigest("one two three", []byte(`{"x":1}`)); got != want {
		t.Errorf("OutputHash = %s, want %s", got, want)
	}
	if NewOutputHash().Sum(nil) != OutputDigest("", nil) {
		t.Error("an empty OutputHash differs from the empty digest")
	}
}
