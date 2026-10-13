// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// maxTemplateImportBody is the document's own cap plus the request envelope and
// the escaping a JSON string adds to a document that is itself at the cap.
const maxTemplateImportBody = 2*maxTemplateDocumentBytes + 4096

// handleImportTemplate answers POST /api/v1/templates/import: read one document
// the way every door does and say what is wrong with it, or hand it back
// normalised. It stores nothing and grants nothing, so it needs no authority
// beyond being signed in; a draft built from the answer is checked again where
// it is used.
func (s *Server) handleImportTemplate(w http.ResponseWriter, r *http.Request) {
	body, ok := readCappedBody(w, r, maxTemplateImportBody, "request body")
	if !ok {
		return
	}
	var req client.TemplateImportRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "invalid JSON body: "+err.Error())
		return
	}
	switch req.Kind {
	case "", client.TemplateDocumentKind, client.RunDocumentKind:
	default:
		writeErrorReason(w, http.StatusBadRequest, reasonTemplateFieldInvalid, templateFieldInvalidMsg("kind", "must be Run or RunTemplate"))
		return
	}
	writeJSON(w, http.StatusOK, validateTemplateImport([]byte(req.Source), req.Format, req.Kind))
}
