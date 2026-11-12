// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
)

// MaxReviewedDownloadBytes bounds one reviewed download, as an upload is bounded: the file is hashed when it is
// asked for and copied to a temporary file when it is released.
const MaxReviewedDownloadBytes = 256 << 20

// FileDownloadScope is the requested_scope of an ApprovalFileDownload row: what its deciders see. The file's
// name, size and SHA-256 and who asked, never its content.
type FileDownloadScope struct {
	Name        string `json:"name"`
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
	RequestedBy string `json:"requested_by"`
}

// Valid reports whether s names a file (at most 255 bytes), a size within MaxReviewedDownloadBytes, a lower-case
// hex SHA-256 and who asked.
func (s FileDownloadScope) Valid() bool {
	sum, err := hex.DecodeString(s.SHA256)
	return s.Name != "" && len(s.Name) <= 255 && s.Bytes >= 0 && s.Bytes <= MaxReviewedDownloadBytes &&
		err == nil && len(sum) == 32 && hex.EncodeToString(sum) == s.SHA256 && s.RequestedBy != ""
}

// ParseFileDownloadScope reads a stored scope back: exactly FileDownloadScope's keys, and Valid.
func ParseFileDownloadScope(raw json.RawMessage) (FileDownloadScope, bool) {
	var s FileDownloadScope
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&s) != nil || dec.More() || !s.Valid() {
		return FileDownloadScope{}, false
	}
	return s, true
}
