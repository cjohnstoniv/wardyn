// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistant

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func sum(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// PromptDigest is the sha256 of the exact record bytes a turn sent.
func PromptDigest(records string) string { return sum([]byte(records)) }

// OutputDigest is the sha256 of the assistant's text followed by the proposal JSON it returned.
func OutputDigest(text string, proposal []byte) string { return sum([]byte(text), proposal) }

// OutputHash builds OutputDigest a piece at a time, so a long turn's text is never held whole: Text for each
// piece in order, then Sum with the proposal.
type OutputHash struct{ h hash.Hash }

// NewOutputHash is an empty OutputHash.
func NewOutputHash() *OutputHash { return &OutputHash{h: sha256.New()} }

// Text adds the next piece of the assistant's text.
func (o *OutputHash) Text(s string) { o.h.Write([]byte(s)) }

// Sum is OutputDigest of every piece so far, followed by proposal.
func (o *OutputHash) Sum(proposal []byte) string {
	o.h.Write(proposal)
	return hex.EncodeToString(o.h.Sum(nil))
}

// RecordBytes is the records as the first message carries them, section by section.
func RecordBytes(sections []types.AssistantSection) string {
	var b strings.Builder
	for _, s := range sections {
		b.WriteString("## ")
		b.WriteString(s.Label)
		if s.Untrusted {
			b.WriteString(" (untrusted)")
		}
		b.WriteString("\n")
		b.WriteString(s.Text)
		b.WriteString("\n\n")
	}
	return b.String()
}

// RecordsDigest is the digest of the records alone.
func RecordsDigest(sections []types.AssistantSection) string {
	return PromptDigest(RecordBytes(sections))
}

// PreviewDigest covers the destination as well as the records: the serving binding in canonical JSON,
// then the records' digest. The destination is part of what the person confirmed, so a provider or model
// change between preview and start changes the digest even when every record byte is the same.
func PreviewDigest(b types.AssistantBinding, sections []types.AssistantSection) string {
	js, _ := json.Marshal(b) // a struct of strings: marshalling cannot fail
	return sum(js, []byte{0}, []byte(RecordsDigest(sections)))
}
