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
	"slices"
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
//
// A signature signs in to the grant's own SSH host only: a host-bound sign-in
// must name a host key of the grant's known_hosts or of the forge's pinned
// keys, a plain one is signed only for a forge with no host-bound sign-in
// (Azure DevOps), and while the agent holds a key a CONNECT tunnel either side
// opens with an SSH identification goes only to a held grant's endpoint
// (sshAgents.sshTunnel). That reads banners only: SSH inside TLS, or after
// lines sent before the banner, is not seen.
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
	errSSHAgentHost    = errors.New("wardyn: this agent signs in only to its grant's own SSH host")
	errSSHAgentTunnel  = errors.New("wardyn: an SSH session to another host is open; this agent signs in only to its grant's own SSH host")
	errSSHTunnel       = errors.New("wardyn: while the run's ssh agent holds a key, SSH goes only to its grants' own SSH hosts")
)

// pinnedSSHHostKeys are the forges' published host keys, as the agent images
// bake them into /etc/ssh/ssh_known_hosts (TestPinnedSSHHostKeysMatchImage).
const pinnedSSHHostKeys = "github.com,ssh.github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n" +
	"github.com,ssh.github.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg=\n" +
	"github.com,ssh.github.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgV" +
	"smerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVr" +
	"uj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6Cm" +
	"K2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk=\n" +
	"ssh.dev.azure.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7Hr1oTWqNqOlzGJOfGJ4NakVyIzf1rXYd4d7wo6jBlkLvCA4odBlL0mDUyZ0/QUfTTqeu+tm22gOsv+VrVTMk6vwRU75gY/y9ut5Mb3bR5BV58dKXyq9A9UeB5Cakehn5" +
	"Zgm6x1mKoVyf+FFn26iYqXJRgzIZZcZ5V6hrE0Qg39kZm4az48o0AUbf6Sp4SLdvnuMa2sVNwHBboS7EJkm57XQPVU3/QpyNLHbWDdzwtrlS+ez30S3AdYhLKEOxAG8weOnyrtLJAUen9mTkol8oII1edf7mWWbW" +
	"Vf0nBmly21+nZcmCTISQBtdcyPaEno7fFQMDD26/s0lfKob4Kw8H\n"

// sshEndpoint is the SSH-over-443 endpoint agent-run dials for a grant's host
// (ssh_endpoint in deploy/images/claude-code/agent-run), and whether that
// forge signs in only with the host-bound method. ok is false for any other host.
func sshEndpoint(host string) (endpoint string, hostbound, ok bool) {
	switch strings.ToLower(strings.TrimSuffix(host, ".")) {
	case "github.com", "ssh.github.com":
		return "ssh.github.com:443", true, true
	case "dev.azure.com", "ssh.dev.azure.com":
		return "ssh.dev.azure.com:443", false, true
	}
	return "", false, false
}

// knownHostKeys is the wire form of every host key a known_hosts text lists
// for host, or for any host when host is "". Marked lines (@revoked,
// @cert-authority) name no host key and are skipped.
func knownHostKeys(text, host string) [][]byte {
	var out [][]byte
	for rest := []byte(text); len(rest) > 0; {
		marker, hosts, key, _, next, err := ssh.ParseKnownHosts(rest)
		if err != nil {
			break
		}
		if marker == "" && (host == "" || slices.Contains(hosts, host)) {
			out = append(out, key.Marshal())
		}
		rest = next
	}
	return out
}

// sshAgents holds the keys of the run's ssh_key grants, each minted on first
// use and kept until close. mu also single-flights the mint.
type sshAgents struct {
	mu     sync.Mutex
	keys   map[uuid.UUID]*sshAgentKey
	closed bool
	// tunnels are the live CONNECT tunnels seen carrying SSH, by "host:port".
	tunnels map[*tunnelWatch]string
}

type sshAgentKey struct {
	ring       agent.ExtendedAgent
	blob       []byte // the public key, wire form
	user       string // the grant's SSH user; "git" is always accepted
	knownHosts string
	endpoint   string   // the grant's SSH endpoint, "ssh.github.com:443"
	hostKeys   [][]byte // the host keys a host-bound sign-in may name
	unbound    bool     // the forge has no host-bound sign-in, so a plain one is signed
}

// sshTunnel records a live CONNECT tunnel to target that carries SSH. While
// the agent holds a key it refuses one whose target is no held grant's
// endpoint.
func (s *sshAgents) sshTunnel(w *tunnelWatch, target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed && len(s.keys) > 0 && !s.heldEndpoint(target) {
		return errSSHTunnel
	}
	if s.tunnels == nil {
		s.tunnels = map[*tunnelWatch]string{}
	}
	s.tunnels[w] = target
	return nil
}

// dropTunnel forgets a tunnel that ended.
func (s *sshAgents) dropTunnel(w *tunnelWatch) {
	s.mu.Lock()
	delete(s.tunnels, w)
	s.mu.Unlock()
}

// heldEndpoint reports whether target is a held grant's endpoint. s.mu is held.
func (s *sshAgents) heldEndpoint(target string) bool {
	for _, k := range s.keys {
		if k.endpoint == target {
			return true
		}
	}
	return false
}

// sshOnlyToGrants reports whether every live SSH tunnel goes to a held
// grant's endpoint, so a session opened before a key was held cannot borrow
// a host-bound signature (its host key binds it to one of them).
func (s *sshAgents) sshOnlyToGrants() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tunnels {
		if !s.heldEndpoint(t) {
			return false
		}
	}
	return true
}

// sshOnlyTo reports whether every live SSH tunnel goes to endpoint: a plain
// sign-in names no host, so it is signed only beside its own grant's sessions.
func (s *sshAgents) sshOnlyTo(endpoint string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tunnels {
		if t != endpoint {
			return false
		}
	}
	return true
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
	a := &userauthAgent{key: key, agents: &p.sshAgent}
	var out bytes.Buffer
	if err := agent.ServeAgent(a, struct {
		io.Reader
		io.Writer
	}{bytes.NewReader(frame), &out}); !errors.Is(err, io.EOF) || out.Len() == 0 {
		p.emitLocalDecision(r, egress.Deny, ruleSourceMint, nil)
		http.Error(w, "wardyn: malformed ssh agent request", http.StatusBadRequest)
		return
	}
	decision, cause := egress.Allow, ""
	if a.refused != nil {
		decision, cause = egress.Deny, a.refused.Error()
	}
	p.emitLocalDecisionCause(r, decision, ruleSourceMint, nil, cause)
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
	m, err := p.sshKeyMaterial(ctx, id)
	if err != nil {
		return nil, err
	}
	procMaskFor(p.maskKey, []byte(m.Token))
	endpoint, hostbound, ok := sshEndpoint(m.Host)
	if !ok {
		return nil, errors.New("the control plane named no SSH host this agent signs in to for this grant (github.com or dev.azure.com)")
	}
	raw, err := ssh.ParseRawPrivateKey([]byte(m.Token))
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
	k := &sshAgentKey{ring: ring, blob: signer.PublicKey().Marshal(), user: m.Username, knownHosts: m.KnownHosts,
		endpoint: endpoint, unbound: !hostbound,
		hostKeys: append(knownHostKeys(m.KnownHosts, ""), knownHostKeys(pinnedSSHHostKeys, strings.TrimSuffix(endpoint, ":443"))...)}
	if s.keys == nil {
		s.keys = map[uuid.UUID]*sshAgentKey{}
	}
	s.keys[id] = k
	return k, nil
}

// sshKeyMaterial is the grant's private key (Token), SSH user, known_hosts
// and SSH host.
func (p *Proxy) sshKeyMaterial(ctx context.Context, id uuid.UUID) (ResidentMint, error) {
	if m, ok := p.residentMints[id]; ok {
		switch {
		case m.Kind != types.GrantSSHKey:
			return ResidentMint{}, errors.New("this grant is not an ssh_key")
		case m.ExpiresAt.IsZero() || !p.now().Before(m.ExpiresAt):
			return ResidentMint{}, errors.New("this runner_resident credential has expired on this runner; start a new run to use it again")
		}
		return m, nil
	}
	reqBody, _ := json.Marshal(map[string]string{"grant_id": id.String()})
	resp, err := p.forwardToControlPlane(ctx, http.MethodPost, "/api/v1/internal/credentials/mint", reqBody, "application/json")
	if err != nil {
		return ResidentMint{}, fmt.Errorf("mint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBrokeredBody))
	if resp.StatusCode != http.StatusOK {
		return ResidentMint{}, fmt.Errorf("mint status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var mr struct {
		Kind       types.GrantKind `json:"kind"`
		Token      string          `json:"token"`
		Username   string          `json:"username"`
		KnownHosts string          `json:"known_hosts"`
		Host       string          `json:"host"`
	}
	if err := json.Unmarshal(body, &mr); err != nil || mr.Token == "" || mr.Kind != types.GrantSSHKey {
		return ResidentMint{}, errors.New("this grant is not an ssh_key")
	}
	return ResidentMint{GrantID: id, Kind: mr.Kind, Token: mr.Token, Username: mr.Username, KnownHosts: mr.KnownHosts, Host: mr.Host}, nil
}

// userauthAgent is the read-only agent one request is served by: it lists
// the grant's key and signs only that key's SSH user-authentication request
// as "git" (what agent-run's ssh config signs in as) or the grant's own
// username, to the grant's own SSH host. refused records why a sign was refused.
type userauthAgent struct {
	key     *sshAgentKey
	agents  *sshAgents
	refused error
}

func (a *userauthAgent) List() ([]*agent.Key, error) { return a.key.ring.List() }

func (a *userauthAgent) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	return a.SignWithFlags(key, data, 0)
}

func (a *userauthAgent) SignWithFlags(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	hostKey, ok := isUserauthRequest(data, a.key.user, a.key.blob)
	switch {
	case !bytes.Equal(key.Marshal(), a.key.blob) || !ok:
		a.refused = errSSHAgentRefused
	case hostKey == nil && !a.key.unbound,
		hostKey != nil && !slices.ContainsFunc(a.key.hostKeys, func(k []byte) bool { return bytes.Equal(k, hostKey) }):
		a.refused = errSSHAgentHost
	case hostKey != nil && !a.agents.sshOnlyToGrants(), hostKey == nil && !a.agents.sshOnlyTo(a.key.endpoint):
		a.refused = errSSHAgentTunnel
	default:
		return a.key.ring.SignWithFlags(key, data, flags)
	}
	return nil, a.refused
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
// hostKey is the host-bound form's host key, nil for the plain form.
func isUserauthRequest(data []byte, user string, blob []byte) (hostKey []byte, ok bool) {
	fields := make([][]byte, 0, 8)
	rest := data
	// next reads k more strings into fields.
	next := func(k int) bool {
		for range k {
			if len(rest) < 4 {
				return false
			}
			n := binary.BigEndian.Uint32(rest)
			if uint64(n) > uint64(len(rest)-4) {
				return false
			}
			fields, rest = append(fields, rest[4:4+n]), rest[4+n:]
		}
		return true
	}
	if !next(1) || len(rest) == 0 || rest[0] != 50 {
		return nil, false
	}
	rest = rest[1:]
	if !next(3) || len(rest) == 0 || rest[0] != 1 {
		return nil, false
	}
	rest = rest[1:]
	if !next(2) {
		return nil, false
	}
	method := string(fields[3])
	switch method {
	case "publickey":
	case "publickey-hostbound-v00@openssh.com":
		if !next(1) || len(fields[6]) == 0 {
			return nil, false
		}
		hostKey = fields[6]
	default:
		return nil, false
	}
	who := string(fields[1])
	ok = len(rest) == 0 && len(fields[0]) > 0 && (who == "git" || (user != "" && who == user)) &&
		string(fields[2]) == "ssh-connection" && len(fields[4]) > 0 && bytes.Equal(fields[5], blob)
	return hostKey, ok
}
