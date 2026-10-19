// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// testSSHKey is a throwaway key: its PEM and its public half.
func testSSHKey(t *testing.T, rsaKey bool) (string, ssh.PublicKey) {
	t.Helper()
	var priv any
	if rsaKey {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		priv = k
	} else {
		_, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		priv = k
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block)), signer.PublicKey()
}

// sshAgentProxy is a proxy whose control plane answers a mint of sshGrant
// with keyPEM and of any other grant with a git_pat, counting the mints.
func sshAgentProxy(t *testing.T, sshGrant uuid.UUID, keyPEM string, opts func(*Options)) (*Proxy, *bytes.Buffer, *atomic.Int32) {
	t.Helper()
	var mints atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mints.Add(1)
		var req struct {
			GrantID uuid.UUID `json:"grant_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.GrantID == sshGrant {
			_ = json.NewEncoder(w).Encode(map[string]string{"kind": "ssh_key", "token": keyPEM, "username": "git", "known_hosts": "forge.test ssh-ed25519 AAAAhostkey"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"kind": "git_pat", "token": "pat-value", "username": "pat"})
	}))
	t.Cleanup(cp.Close)
	buf := &bytes.Buffer{}
	o := Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		Sink:            &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(cp)),
		ControlPlaneURL: "http://wardynd.test:8080",
		RunToken:        newTokenSource("RUNTOK"),
	}
	if opts != nil {
		opts(&o)
	}
	return newProxy(o), buf, &mints
}

// agentConn carries an agent client's frames to the proxy's route, one
// request per frame, as wardyn-git-helper ssh-exec does.
type agentConn struct {
	t     *testing.T
	p     *Proxy
	grant uuid.UUID
	reply bytes.Buffer
}

func (c *agentConn) Write(b []byte) (int, error) {
	rec := httptest.NewRecorder()
	c.p.ServeHTTP(rec, mustLocalReq(c.t, http.MethodPost, routeSSHAgent+c.grant.String(), bytes.NewReader(b)))
	if rec.Code != http.StatusOK {
		return 0, io.ErrClosedPipe
	}
	c.reply.Write(rec.Body.Bytes())
	return len(b), nil
}

func (c *agentConn) Read(b []byte) (int, error) { return c.reply.Read(b) }

func sshString(s []byte) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(s))), s...)
}

// userauthData is what an SSH client signs to sign in as user with pub.
func userauthData(user string, pub ssh.PublicKey, method string, extra ...[]byte) []byte {
	d := append(sshString([]byte("session-id")), 50)
	for _, f := range [][]byte{[]byte(user), []byte("ssh-connection"), []byte(method)} {
		d = append(d, sshString(f)...)
	}
	d = append(d, 1)
	d = append(d, sshString([]byte(pub.Type()))...)
	d = append(d, sshString(pub.Marshal())...)
	for _, e := range extra {
		d = append(d, sshString(e)...)
	}
	return d
}

// The proxy's agent lists the grant's key and signs its SSH sign-in, minting
// once; it refuses to sign anything else, and the decision log says which.
func TestSSHAgent_SignsUserauthOnly(t *testing.T) {
	for _, rsaKey := range []bool{false, true} {
		grant := uuid.New()
		keyPEM, pub := testSSHKey(t, rsaKey)
		p, buf, mints := sshAgentProxy(t, grant, keyPEM, nil)
		client := agent.NewClient(&agentConn{t: t, p: p, grant: grant})

		keys, err := client.List()
		if err != nil || len(keys) != 1 || !bytes.Equal(keys[0].Marshal(), pub.Marshal()) {
			t.Fatalf("List = %v, %v; want the grant's one key", keys, err)
		}
		data := userauthData("git", pub, "publickey")
		var sig *ssh.Signature
		if rsaKey {
			sig, err = client.(agent.ExtendedAgent).SignWithFlags(pub, data, agent.SignatureFlagRsaSha256)
		} else {
			sig, err = client.Sign(pub, data)
		}
		if err != nil || pub.Verify(data, sig) != nil {
			t.Fatalf("sign the sign-in (rsa=%v): %v", rsaKey, err)
		}
		if rsaKey && sig.Format != ssh.KeyAlgoRSASHA256 {
			t.Fatalf("rsa signature format = %q, want %q", sig.Format, ssh.KeyAlgoRSASHA256)
		}
		if d := lastDecision(t, buf); d.Decision != egress.Allow || d.RuleSource != ruleSourceMint {
			t.Fatalf("sign decision = %+v, want a mint allow", d)
		}
		hostbound := userauthData("git", pub, "publickey-hostbound-v00@openssh.com", []byte("hostkey"))
		if _, err := client.Sign(pub, hostbound); err != nil {
			t.Fatalf("sign the host-bound sign-in: %v", err)
		}
		for name, d := range map[string][]byte{
			"arbitrary data":  []byte("a commit to sign"),
			"another user":    userauthData("root", pub, "publickey"),
			"another method":  userauthData("git", pub, "hostbased"),
			"trailing bytes":  append(userauthData("git", pub, "publickey"), 0),
			"hostbound short": userauthData("git", pub, "publickey-hostbound-v00@openssh.com"),
		} {
			if _, err := client.Sign(pub, d); err == nil {
				t.Fatalf("%s: signed, want a refusal", name)
			}
			if dl := lastDecision(t, buf); dl.Decision != egress.Deny {
				t.Fatalf("%s: decision = %+v, want a deny", name, dl)
			}
		}
		if err := client.Add(agent.AddedKey{PrivateKey: ed25519.NewKeyFromSeed(make([]byte, 32))}); err == nil {
			t.Fatal("Add: want a refusal from a read-only agent")
		}
		if n := mints.Load(); n != 1 {
			t.Fatalf("control-plane mints = %d, want 1", n)
		}
	}
}

// The known_hosts route answers the mint's known_hosts; close ends the agent
// for good and drops the key, with no further mint.
func TestSSHAgent_KnownHostsAndClose(t *testing.T) {
	grant := uuid.New()
	keyPEM, _ := testSSHKey(t, false)
	p, buf, mints := sshAgentProxy(t, grant, keyPEM, nil)
	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, routeSSHAgent+grant.String()+sshAgentKnownHosts, nil))
		return rec
	}
	if rec := get(); rec.Code != http.StatusOK || rec.Body.String() != "forge.test ssh-ed25519 AAAAhostkey" {
		t.Fatalf("known_hosts: %d %q", rec.Code, rec.Body.String())
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, routeSSHAgentClose, nil))
	if rec.Code != http.StatusNoContent || p.sshAgent.keys != nil {
		t.Fatalf("close: %d, keys held %d", rec.Code, len(p.sshAgent.keys))
	}
	if rec := get(); rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "PRIVATE") {
		t.Fatalf("known_hosts after close: %d %q, want 403", rec.Code, rec.Body.String())
	}
	if _, err := agent.NewClient(&agentConn{t: t, p: p, grant: grant}).List(); err == nil {
		t.Fatal("List after close: want a refusal")
	}
	if d := lastDecision(t, buf); d.Decision != egress.Deny {
		t.Fatalf("after close decision = %+v, want a deny", d)
	}
	if n := mints.Load(); n != 1 {
		t.Fatalf("control-plane mints = %d, want 1", n)
	}
}

// The raw mint relay never hands the sandbox an ssh_key: the answer is
// withheld with a deny, while another kind passes through.
func TestBrokerMint_SSHKeyAnswerWithheld(t *testing.T) {
	grant, pat := uuid.New(), uuid.New()
	keyPEM, _ := testSSHKey(t, false)
	p, buf, _ := sshAgentProxy(t, grant, keyPEM, nil)
	rec := residentMintCall(t, p, grant)
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "PRIVATE KEY") {
		t.Fatalf("raw ssh_key mint: %d %q, want a 403 with no key", rec.Code, rec.Body.String())
	}
	if d := lastDecision(t, buf); d.Decision != egress.Deny || d.RuleSource != ruleSourceMint {
		t.Fatalf("raw ssh_key mint decision = %+v, want a mint deny", d)
	}
	if rec := residentMintCall(t, p, pat); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pat-value") {
		t.Fatalf("raw git_pat mint: %d %q, want it passed through", rec.Code, rec.Body.String())
	}
}

// A grant that is not an ssh_key gets no agent and leaks nothing; a grant a
// broker mints is refused before any mint.
func TestSSHAgent_RefusesOtherGrants(t *testing.T) {
	sshGrant, other, brokered := uuid.New(), uuid.New(), uuid.New()
	keyPEM, _ := testSSHKey(t, false)
	p, _, mints := sshAgentProxy(t, sshGrant, keyPEM, func(o *Options) { o.BrokeredPATGrantIDs = []uuid.UUID{brokered} })
	frame := binary.BigEndian.AppendUint32(nil, 1)
	frame = append(frame, 11) // SSH_AGENTC_REQUEST_IDENTITIES
	for _, tc := range []struct {
		grant uuid.UUID
		code  int
		mints int32
	}{{other, http.StatusBadGateway, 1}, {brokered, http.StatusForbidden, 1}} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, routeSSHAgent+tc.grant.String(), bytes.NewReader(frame)))
		if rec.Code != tc.code || strings.Contains(rec.Body.String(), "pat-value") || mints.Load() != tc.mints {
			t.Fatalf("grant %s: %d %q after %d mints, want %d and %d mints", tc.grant, rec.Code, rec.Body.String(), mints.Load(), tc.code, tc.mints)
		}
	}
}

// A runner_resident ssh_key is signed with from the runner-held value, with
// no control-plane mint, and not at or past its ExpiresAt.
func TestSSHAgent_ResidentKey(t *testing.T) {
	grant, late := uuid.New(), uuid.New()
	keyPEM, pub := testSSHKey(t, false)
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	expires := time.Now().Add(time.Hour)
	p, _, mints := sshAgentProxy(t, uuid.New(), "", func(o *Options) {
		o.Now = func() time.Time { return time.Unix(0, now.Load()) }
	})
	p.residentMints = residentMintsByGrant([]ResidentMint{
		{GrantID: grant, Kind: types.GrantSSHKey, Username: "git", Token: keyPEM, ExpiresAt: expires},
		{GrantID: late, Kind: types.GrantSSHKey, Username: "git", Token: keyPEM, ExpiresAt: expires},
	})
	data := userauthData("git", pub, "publickey")
	if sig, err := agent.NewClient(&agentConn{t: t, p: p, grant: grant}).Sign(pub, data); err != nil || pub.Verify(data, sig) != nil {
		t.Fatalf("resident sign: %v", err)
	}
	now.Store(expires.UnixNano())
	if _, err := agent.NewClient(&agentConn{t: t, p: p, grant: late}).List(); err == nil {
		t.Fatal("resident key at expiry: want a refusal")
	}
	if n := mints.Load(); n != 0 {
		t.Fatalf("control-plane mints = %d, want none", n)
	}
}
