// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ResidentMint is one runner_resident ssh_key or git_pat grant whose value a
// runner's proxy holds (design 0.9 §7.4): the sandbox's git credential helper
// mints a git_pat through this proxy's mint route and gets the runner-held
// value back; an ssh_key is signed with by this proxy's ssh agent and never
// answered (ssh_agent.go). ExpiresAt is the earlier of the delivery's
// not_after and the credential's own expiry; at or after it, or when it is
// zero, the proxy answers nothing.
type ResidentMint struct {
	GrantID    uuid.UUID       `json:"grant_id"`
	Kind       types.GrantKind `json:"kind"`
	Username   string          `json:"username,omitempty"`
	KnownHosts string          `json:"known_hosts,omitempty"`
	Token      string          `json:"token"`
	ExpiresAt  time.Time       `json:"expires_at"`
}

// validResidentMints refuses a resident_mints entry with no grant, a kind
// other than ssh_key or git_pat, or a grant named twice.
func validResidentMints(ms []ResidentMint) error {
	seen := map[uuid.UUID]bool{}
	for i, m := range ms {
		switch {
		case m.GrantID == uuid.Nil:
			return fmt.Errorf("config: resident_mints[%d] needs a grant_id", i)
		case m.Kind != types.GrantSSHKey && m.Kind != types.GrantGitPAT:
			return fmt.Errorf("config: resident_mints[%d]: kind %q is never answered locally", i, m.Kind)
		case seen[m.GrantID]:
			return fmt.Errorf("config: resident_mints names grant %s twice", m.GrantID)
		}
		seen[m.GrantID] = true
	}
	return nil
}

func residentMintsByGrant(ms []ResidentMint) map[uuid.UUID]ResidentMint {
	if len(ms) == 0 {
		return nil
	}
	out := make(map[uuid.UUID]ResidentMint, len(ms))
	for _, m := range ms {
		out[m.GrantID] = m
	}
	return out
}

// residentMint is the runner_resident grant a sandbox-supplied mint body names,
// decoded as isBrokeredPATGrant decodes it; an undecodable body names none and
// is relayed for the control plane to refuse.
func (p *Proxy) residentMint(body []byte) (ResidentMint, bool) {
	if len(p.residentMints) == 0 {
		return ResidentMint{}, false
	}
	var req struct {
		GrantID uuid.UUID `json:"grant_id"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil || req.GrantID == uuid.Nil {
		return ResidentMint{}, false
	}
	m, ok := p.residentMints[req.GrantID]
	return m, ok
}

// answerResidentMint answers a mint of a runner_resident grant with the
// runner-held value, in the control plane's mint answer shape, and records the
// local decision. At or past ExpiresAt, or with none, it answers a 403 and a
// deny: nothing reads the value after its not_after (owner ruling Q1b). The
// answer carries ExpiresAt, so a caller's cache lapses with it.
func (p *Proxy) answerResidentMint(w http.ResponseWriter, r *http.Request, m ResidentMint) {
	if m.ExpiresAt.IsZero() || !p.now().Before(m.ExpiresAt) {
		p.emitLocalDecision(r, egress.Deny, ruleSourceMint, nil)
		http.Error(w, "wardyn: this runner_resident credential has expired on this runner; start a new run to use it again", http.StatusForbidden)
		return
	}
	// A resident ssh_key is signed with by the proxy's ssh agent and never
	// answered to the sandbox (ssh_agent.go).
	if m.Kind == types.GrantSSHKey {
		p.emitLocalDecision(r, egress.Deny, ruleSourceMint, nil)
		http.Error(w, sshKeyMintRefused, http.StatusForbidden)
		return
	}
	p.emitLocalDecision(r, egress.Allow, ruleSourceMint, nil)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Kind       types.GrantKind `json:"kind"`
		Token      string          `json:"token"`
		Username   string          `json:"username,omitempty"`
		ExpiresAt  string          `json:"expires_at"`
		KnownHosts string          `json:"known_hosts,omitempty"`
	}{m.Kind, m.Token, m.Username, m.ExpiresAt.UTC().Format(time.RFC3339), m.KnownHosts})
}

