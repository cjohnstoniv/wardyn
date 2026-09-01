// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Self-service SSH key management: GET/POST/DELETE /api/v1/me/ssh-keys[/{fingerprint}].
// This is the REST half of C3; the gateway that AUTHENTICATES against these
// rows lives in sshgateway.go. Any authenticated human manages their OWN keys
// (scoped by principal, both at the store and here) — this is deliberately
// NOT operator-gated, unlike secret/policy/workspace writes: an SSH key is a
// personal credential binding, not deployment configuration.
package api

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sshMaxKeysPerPrincipal bounds how many keys one human may register — a
// small, generous cap against an accidental (or scripted) unbounded-add loop;
// remove an old key first past this.
const sshMaxKeysPerPrincipal = 20

// addSSHKeyRequest is the POST /api/v1/me/ssh-keys body.
type addSSHKeyRequest struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

// handleListSSHKeys is GET /api/v1/me/ssh-keys: the caller's own registered
// keys. There is no admin/operator view of another principal's keys.
func (s *Server) handleListSSHKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.cfg.Store.ListSSHKeysByPrincipal(r.Context(), principalFromRequest(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list ssh keys: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

// handleAddSSHKey is POST /api/v1/me/ssh-keys: register a public key against
// the caller's own principal. This endpoint is the gateway's ENTIRE trust
// root (sshgateway.go's PublicKeyCallback authenticates against nothing else),
// so validation here fails closed: unparseable input, private-key material,
// and more-than-one-key input are all refused (422), never stored.
func (s *Server) handleAddSSHKey(w http.ResponseWriter, r *http.Request) {
	var req addSSHKeyRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	pk, comment, msg := parseSSHAuthorizedKeyLine(req.PublicKey)
	if msg != "" {
		writeError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = comment
	}
	principal := principalFromRequest(r)
	if principal == adminTokenPrincipal && s.cfg.OIDC != nil {
		// A key registered under the non-human admin-token principal can NEVER
		// authorize an SSO human's run: sshAuth's owner-only gate compares
		// run.CreatedBy (the OIDC sub) against the key's stored Principal, and
		// "admin-token" matches no run a human ever creates. With SSO
		// configured, only a signed-in human's own POST — which
		// principalFromRequest resolves to their OIDC sub — can register a
		// key that will ever work. Reject before writing one that would sit
		// dead in the store forever (docs/SSH.md's admin-token/CI-only note).
		writeError(w, http.StatusUnprocessableEntity,
			"a key registered with the admin token can never authorize an SSO-signed-in human's run — sign in to the console and add the key from Account -> SSH keys instead")
		return
	}

	existing, err := s.cfg.Store.ListSSHKeysByPrincipal(r.Context(), principal)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list ssh keys: "+err.Error())
		return
	}
	if len(existing) >= sshMaxKeysPerPrincipal {
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("too many registered keys (max %d) — remove one first", sshMaxKeysPerPrincipal))
		return
	}

	// The key's role is stamped HERE, from the registering session's own role,
	// exactly the way handleAttachTicket stamps a ticket at mint time: SSH
	// carries no session cookie, so the gateway has no live requireOperator to
	// consult at connect time and this stamp is its ONLY role source at
	// registration (migration 0043). Ceiling, documented in docs/SSH.md
	// §Bounds: BOUNDED-STALE, not live — a demotion reaches this key only at
	// its owner's next OIDC login (oidc.Config.OnLogin re-stamps role AND
	// role_checked_at for every key the principal owns) or once
	// role_checked_at exceeds WARDYN_SSH_ROLE_TTL (migration 0046), whichever
	// comes first — never instantly, and never without one of those two.
	//
	// DELIBERATELY isOperator, and a security admin's key stamps member. This
	// field means exactly "reaches runs its holder does not own"
	// (sshgateway.go's == oidc.RoleAdmin check), not the registering session's
	// tier — the asymmetry the three-tier model exists to express
	// (internal/auth/oidc's RoleSecurityAdmin). A ladder here would put an
	// interactive shell in every developer's sandbox.
	role := oidc.RoleMember
	if s.isOperator(r.Context()) {
		role = oidc.RoleAdmin
	}
	// now is both CreatedAt and RoleCheckedAt: registration IS a role check —
	// role above was just read from this same request's live session — so a
	// freshly-registered key must not read as stale (migration 0046) before
	// its owner's next login ever gets a chance to refresh it.
	now := s.cfg.Now().UTC()

	k := types.SSHPublicKey{
		// FingerprintSHA256 + MarshalAuthorizedKey are both computed from the
		// PARSED key, never echoing the caller's raw bytes back into storage —
		// whitespace/options noise in the pasted line canonicalizes away.
		Fingerprint:   ssh.FingerprintSHA256(pk),
		Principal:     principal,
		Name:          name,
		PublicKey:     strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(pk)), "\n"),
		Role:          role,
		RoleCheckedAt: &now,
		CreatedAt:     now,
	}
	added, err := s.cfg.Store.AddSSHKey(r.Context(), k)
	if errors.Is(err, store.ErrConflict) {
		// Generic on purpose: the fingerprint PK is GLOBAL (correct — a key
		// must map to exactly one principal), so this 409 can legitimately
		// mean "you already added it" OR "someone else holds it". Naming
		// "already registered" would confirm the SECOND case to a caller who
		// does not own it — key-squatting reconnaissance. See docs/SSH.md's
		// remediation section and THREAT-MODEL.md's residual for the
		// operator-side fix (there is no self-service one by design).
		writeError(w, http.StatusConflict, "unable to register this key")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "add ssh key: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principal,
		"ssh_key.add", added.Fingerprint, "success", mustJSON(map[string]any{"name": added.Name})))
	writeJSON(w, http.StatusCreated, added)
}

// handleDeleteSSHKey is DELETE /api/v1/me/ssh-keys/{fingerprint}: remove one
// of the caller's own keys. Store.DeleteSSHKey scopes the DELETE to principal,
// so a fingerprint registered by someone else 404s exactly like one that
// never existed — no existence leak across principals.
//
// The fingerprint is ssh.FingerprintSHA256's raw-base64 form, which routinely
// contains '/' — a caller MUST percent-encode it as one path segment
// (encodeURIComponent client-side), and chi correspondingly prefers
// r.URL.RawPath for route MATCHING (so an encoded '/' is never mistaken for
// an extra path segment) but returns the URLParam value RAW, still encoded —
// unescape it here, or a legitimately-encoded fingerprint never matches the
// stored (decoded) one. PathUnescape, not QueryUnescape: this is a path
// segment, where a literal '+' must stay '+', never become a space.
func (s *Server) handleDeleteSSHKey(w http.ResponseWriter, r *http.Request) {
	fp, err := url.PathUnescape(chi.URLParam(r, "fingerprint"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid fingerprint encoding")
		return
	}
	principal := principalFromRequest(r)
	err = s.cfg.Store.DeleteSSHKey(r.Context(), fp, principal)
	if notFoundIf(w, err, "ssh key") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete ssh key: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principal,
		"ssh_key.delete", fp, "success", nil))
	w.WriteHeader(http.StatusNoContent)
}

// parseSSHAuthorizedKeyLine validates raw as ONE SSH public key (an
// authorized_keys line) and returns the parsed key plus its trailing comment,
// or a non-empty, caller-facing msg naming why it was refused. Private-key
// material gets a specific, actionable message rather than the generic parse
// error; trailing content after the one key (a second pasted key, stray text)
// is refused too — this validates the gateway's entire trust root, so a
// caller must be unambiguous about which single key they mean.
func parseSSHAuthorizedKeyLine(raw string) (pk ssh.PublicKey, comment, msg string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, "", "public_key is required"
	}
	if strings.Contains(trimmed, "PRIVATE KEY") || strings.Contains(trimmed, "PuTTY-User-Key-File") {
		return nil, "", "this looks like a PRIVATE key — paste your PUBLIC key instead (e.g. the contents of ~/.ssh/id_ed25519.pub)"
	}
	parsed, cmt, _, rest, err := ssh.ParseAuthorizedKey([]byte(trimmed))
	if err != nil {
		return nil, "", "not a valid SSH public key: " + err.Error()
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, "", "paste exactly one public key line"
	}
	return parsed, cmt, ""
}
