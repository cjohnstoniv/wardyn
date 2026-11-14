// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// A PUT of a secret is an upsert, so a client that decided "this name is new" from a list it read earlier would
// replace a value someone else stored since. If-None-Match: * on PUT /secrets/{name} (and the explicit
// /me/secrets/{name} and /shared-secrets/{name}, which share putSecretFor) makes the write create-only: the store
// refuses it when the namespace already holds the name, and the client can ask before replacing it. Every pg Put
// path honours it; a store that keeps no revision (a test double) ignores it and upserts.

// secretExistsRefusal answers a create-only PUT of a name its namespace already holds.
const secretExistsRefusal = "a secret with this name already exists, so nothing was stored; send the write without If-None-Match: * to replace it"

// secretCreateOnly is ctx with secretstore.WithIfRevision(ctx, "") (the row must not exist) when r carries
// If-None-Match: *; any other request keeps the plain upsert.
func secretCreateOnly(ctx context.Context, r *http.Request) context.Context {
	if strings.TrimSpace(r.Header.Get("If-None-Match")) != "*" {
		return ctx
	}
	return secretstore.WithIfRevision(ctx, "")
}

// refuseSecretExists answers 409 secret_exists for a create-only Put that found the name taken; nothing was
// written. writeServerError does not map secretstore.ErrRevisionChanged, so without this it would be a 500.
func refuseSecretExists(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, secretstore.ErrRevisionChanged) {
		return false
	}
	writeErrorReason(w, http.StatusConflict, reasonSecretExists, secretExistsRefusal)
	return true
}
