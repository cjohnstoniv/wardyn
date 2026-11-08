// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// printKubeCall adds a held kubectl write's cluster, namespace, method, path, query, media type and hashes to
// `approval get`, and reports whether a's scope is one. The body is shown by its SHA-256 over the canonical
// bytes the proxy sends; the call key binds it with the rest. Every value comes from the run, so each prints
// quoted and escaped.
func printKubeCall(w io.Writer, a types.ApprovalRequest) bool {
	if !types.IsKubeCallScope(a.RequestedScope) {
		return false
	}
	var sc types.KubeCallScope
	if json.Unmarshal(a.RequestedScope, &sc) != nil {
		return true
	}
	body := "(none)"
	if len(sc.Body) > 0 {
		sum := sha256.Sum256(sc.Body)
		body = hex.EncodeToString(sum[:])
	}
	fmt.Fprintf(w, "CLUSTER:   %s\nNAMESPACE: %s\nMETHOD:    %s\nPATH:      %s\nQUERY:     %s\nMEDIA TYPE: %s\nBODY SHA256: %s\nCALL SHA256: %s\n",
		strconv.QuoteToASCII(sc.Cluster), strconv.QuoteToASCII(sc.Namespace), strconv.QuoteToASCII(sc.Method),
		strconv.QuoteToASCII(sc.Path), strconv.QuoteToASCII(sc.Query), strconv.QuoteToASCII(sc.MediaType), body,
		strconv.QuoteToASCII(sc.CallSHA256))
	return true
}
