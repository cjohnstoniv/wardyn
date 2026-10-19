// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The run's ssh agent: git over SSH authenticates with an ssh_key grant whose
// private key stays in this proxy. The sandbox's ssh reaches it through
// wardyn-git-helper ssh-exec, which bridges each agent frame to
// routeSSHAgent+<grant id> for the length of one ssh process. The proxy signs
// SSH user-authentication requests only, and serves until agent-run closes
// the agent after the run's start-up clones; it never serves again after that.
const (
	routeSSHAgent      = "/wardyn/v1/ssh-agent/"
	routeSSHAgentClose = "/wardyn/v1/ssh-agent/close"
	// sshAgentKnownHosts is GET <route><grant id>/known_hosts: the grant's
	// operator-supplied known_hosts (ssh's KnownHostsCommand).
	sshAgentKnownHosts = "/known_hosts"
	// maxSSHAgentFrame bounds one agent request; a user-auth sign is well
	// under a kilobyte.
	maxSSHAgentFrame = 64 << 10
)

// sshKeyMintRefused answers a sandbox's raw mint of an ssh_key.
const sshKeyMintRefused = "wardyn: an ssh_key's private key never enters the sandbox; " +
	"git over SSH signs in through the proxy's ssh agent during the run's start-up clone"

var (
	errSSHAgentClosed  = errors.New("the ssh agent closed when the run's start-up clone finished")
	errSSHAgentRefused = errors.New("wardyn: this agent signs SSH sign-in requests for its own key only")
)

// sshAgents holds the keys of the run's ssh_key grants, each minted on first
// use and kept until close. mu also single-flights the mint.
type sshAgents struct {
	mu     sync.Mutex
	keys   map[uuid.UUID]*sshAgentKey
	closed bool
}

type sshAgentKey struct {
	ring       agent.ExtendedAgent
	blob       []byte // the public key, wire form
	user       string // the grant's SSH user; "git" is always accepted
	knownHosts string
}

// isSSHKeyMintAnswer reports whether a control-plane mint answer carries an
// ssh_key: the raw mint relay never hands one to the sandbox.
func isSSHKeyMintAnswer(body []byte) bool {
	var m struct {
		Kind types.GrantKind `json:"kind"`
	}
	return json.Unmarshal(body, &m) == nil && m.Kind == types.GrantSSHKey
}

// handleSSHAgent serves POST <route><grant id> (one agent frame) and
// GET <route><grant id>/known_hosts.
func (p *Proxy) handleSSHAgent(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, routeSSHAgent)
	idText, knownHosts := strings.CutSuffix(rest, sshAgentKnownHosts)
	id, err := uuid.Parse(idText)
	if err != nil || (knownHosts && r.Method != http.MethodGet) || (!knownHosts && r.Method != http.MethodPost) {
		p.emitLocalDecision(r, egress.Deny, ruleSourceMint, nil)
		http.Error(w, "wardyn: unknown ssh agent request", http.StatusNotFound)
		return
	}
	// A grant the git broker or the PAT broker mints is never minted here
	// either: that mint is theirs (see handleBrokerMint).
	if grant, _ := json.Marshal(map[string]string{"grant_id": id.String()}); p.isBrokeredGitGrant(grant) || p.isBrokeredPATGrant(grant) {
		p.emitLocalDecision(r, egress.Deny, ruleSourceMint, nil)
		http.Error(w, "wardyn: this grant is brokered on another route and has no ssh agent", http.StatusForbidden)
		return
	}
	var frame []byte
	if !knownHosts {
		if frame, err = io.ReadAll(io.LimitReader(r.Body, maxSSHAgentFrame+1)); err != nil || len(frame) > maxSSHAgentFrame {
			p.emitLocalDecision(r, egress.Deny, ruleSourceMint, nil)
			http.Error(w, "wardyn: ssh agent request too large or unreadable", http.StatusBadRequest)
			return
		}
	}
	key, err := p.sshAgentKey(r.Context(), id)
	if err != nil {
		p.emitLocalDecision(r, egress.Deny, ruleSourceMint, nil)
		status := http.StatusBadGateway
		if errors.Is(err, errSSHAgentClosed) {
			status = http.StatusForbidden
		}
		http.Error(w, "wardyn: ssh agent: "+err.Error(), status)
		return
	}
	if knownHosts {
		p.emitLocalDecision(r, egress.Allow, ruleSourceMint, nil)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, key.knownHosts)
		return
	}
	a := &userauthAgent{key: key}
	var out bytes.Buffer
	if err := agent.ServeAgent(a, struct {
		io.Reader
		io.Writer
	}{bytes.NewReader(frame), &out}); !errors.Is(err, io.EOF) || out.Len() == 0 {
		p.emitLocalDecision(r, egress.Deny, ruleSourceMint, nil)
		http.Error(w, "wardyn: malformed ssh agent request", http.StatusBadRequest)
		return
	}
	decision := egress.Allow
	if a.refused {
		decision = egress.Deny
	}
	p.emitLocalDecision(r, decision, ruleSourceMint, nil)
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(out.Bytes())
}

// handleSSHAgentClose closes the agent for the rest of this proxy's life and
// drops every key it holds. Idempotent.
func (p *Proxy) handleSSHAgentClose(w http.ResponseWriter, _ *http.Request) {
	s := &p.sshAgent
	s.mu.Lock()
	for _, k := range s.keys {
		_ = k.ring.RemoveAll()
	}
	s.keys, s.closed = nil, true
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// sshAgentKey returns the grant's key, minting it on first use: from the
// runner-held value for a runner_resident grant, else once from the control
// plane with the run token.
func (p *Proxy) sshAgentKey(ctx context.Context, id uuid.UUID) (*sshAgentKey, error) {
	s := &p.sshAgent
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errSSHAgentClosed
	}
	if k := s.keys[id]; k != nil {
		return k, nil
	}
	pem, user, knownHosts, err := p.sshKeyMaterial(ctx, id)
	if err != nil {
		return nil, err
	}
	procMaskFor(p.maskKey, []byte(pem))
	raw, err := ssh.ParseRawPrivateKey([]byte(pem))
	if err != nil {
		return nil, errors.New("the stored key could not be read as an unencrypted SSH private key")
	}
	signer, err := ssh.NewSignerFromKey(raw)
	if err != nil {
		return nil, errors.New("the stored key type is not supported for SSH")
	}
	ring, _ := agent.NewKeyring().(agent.ExtendedAgent)
	if err := ring.Add(agent.AddedKey{PrivateKey: raw, Comment: "wardyn ssh_key grant " + id.String()}); err != nil {
		return nil, fmt.Errorf("load the key: %w", err)
	}
	k := &sshAgentKey{ring: ring, blob: signer.PublicKey().Marshal(), user: user, knownHosts: knownHosts}
	if s.keys == nil {
		s.keys = map[uuid.UUID]*sshAgentKey{}
	}
	s.keys[id] = k
	return k, nil
}

// sshKeyMaterial is the grant's private key, SSH user and known_hosts.
func (p *Proxy) sshKeyMaterial(ctx context.Context, id uuid.UUID) (pem, user, knownHosts string, err error) {
	if m, ok := p.residentMints[id]; ok {
		switch {
		case m.Kind != types.GrantSSHKey:
			return "", "", "", errors.New("this grant is not an ssh_key")
		case m.ExpiresAt.IsZero() || !p.now().Before(m.ExpiresAt):
			return "", "", "", errors.New("this runner_resident credential has expired on this runner; start a new run to use it again")
		}
		return m.Token, m.Username, m.KnownHosts, nil
	}
	reqBody, _ := json.Marshal(map[string]string{"grant_id": id.String()})
	resp, err := p.forwardToControlPlane(ctx, http.MethodPost, "/api/v1/internal/credentials/mint", reqBody, "application/json")
	if err != nil {
		return "", "", "", fmt.Errorf("mint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBrokeredBody))
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("mint status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var mr struct {
		Kind       types.GrantKind `json:"kind"`
		Token      string          `json:"token"`
		Username   string          `json:"username"`
		KnownHosts string          `json:"known_hosts"`
	}
	if err := json.Unmarshal(body, &mr); err != nil || mr.Token == "" || mr.Kind != types.GrantSSHKey {
		return "", "", "", errors.New("this grant is not an ssh_key")
	}
	return mr.Token, mr.Username, mr.KnownHosts, nil
}

// userauthAgent is the read-only agent one request is served by: it lists
// the grant's key and signs only that key's SSH user-authentication request
// as "git" (what agent-run's ssh config signs in as) or the grant's own
// username. refused records a refused sign.
type userauthAgent struct {
	key     *sshAgentKey
	refused bool
}

func (a *userauthAgent) List() ([]*agent.Key, error) { return a.key.ring.List() }

func (a *userauthAgent) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	return a.SignWithFlags(key, data, 0)
}

func (a *userauthAgent) SignWithFlags(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	if !bytes.Equal(key.Marshal(), a.key.blob) || !isUserauthRequest(data, a.key.user, a.key.blob) {
		a.refused = true
		return nil, errSSHAgentRefused
	}
	return a.key.ring.SignWithFlags(key, data, flags)
}

func (a *userauthAgent) Add(agent.AddedKey) error       { return errSSHAgentRefused }
func (a *userauthAgent) Remove(ssh.PublicKey) error     { return errSSHAgentRefused }
func (a *userauthAgent) RemoveAll() error               { return errSSHAgentRefused }
func (a *userauthAgent) Lock([]byte) error              { return errSSHAgentRefused }
func (a *userauthAgent) Unlock([]byte) error            { return errSSHAgentRefused }
func (a *userauthAgent) Signers() ([]ssh.Signer, error) { return nil, errSSHAgentRefused }
func (a *userauthAgent) Extension(string, []byte) ([]byte, error) {
	return nil, agent.ErrExtensionUnsupported
}

// isUserauthRequest reports whether data is exactly what an SSH client signs
// to sign in as "git" or user with the key blob (RFC 4252 §7, and OpenSSH's
// host-bound form, which appends the server's host key):
// string session_id, byte 50, string user, string "ssh-connection",
// string method, bool TRUE, string algorithm, string blob [, string host key].
func isUserauthRequest(data []byte, user string, blob []byte) bool {
	fields := make([][]byte, 0, 8)
	rest := data
	next := func() bool {
		if len(rest) < 4 {
			return false
		}
		n := binary.BigEndian.Uint32(rest)
		if uint64(n) > uint64(len(rest)-4) {
			return false
		}
		fields, rest = append(fields, rest[4:4+n]), rest[4+n:]
		return true
	}
	if !next() || len(rest) == 0 || rest[0] != 50 {
		return false
	}
	rest = rest[1:]
	if !next() || !next() || !next() || len(rest) == 0 || rest[0] != 1 {
		return false
	}
	rest = rest[1:]
	if !next() || !next() {
		return false
	}
	method := string(fields[3])
	switch method {
	case "publickey":
	case "publickey-hostbound-v00@openssh.com":
		if !next() {
			return false
		}
	default:
		return false
	}
	who := string(fields[1])
	return len(rest) == 0 && len(fields[0]) > 0 && (who == "git" || (user != "" && who == user)) &&
		string(fields[2]) == "ssh-connection" && len(fields[4]) > 0 && bytes.Equal(fields[5], blob)
}
