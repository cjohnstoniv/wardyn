// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sshAgentInContainerEnv switches TestSSHAgent_DockerClone to its in-container
// half: the test binary re-runs itself inside the agent image, where the real
// OpenSSH client and git are, beside the real wardyn-git-helper.
const sshAgentInContainerEnv = "WARDYN_SSH_AGENT_IN_CONTAINER"

const (
	sshAgentImage       = "wardyn/agent-claude-code:local"
	sshAgentCloneMarker = "ssh-agent-docker-clone: cloned over ssh with no key file"
)

// TestSSHAgent_DockerClone clones over SSH in the agent image, with real ssh
// and git and the real wardyn-git-helper as GIT_SSH_COMMAND, signing in
// through this proxy's ssh agent: the clone succeeds for an ed25519 and an RSA
// grant, no file the sandbox user can write holds either key, the raw mint
// relay hands no key back, and once the agent is closed a clone fails.
func TestSSHAgent_DockerClone(t *testing.T) {
	if os.Getenv(sshAgentInContainerEnv) == "1" {
		sshAgentCloneInContainer(t)
		return
	}
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set; skipping the in-image SSH clone")
	}
	dir := t.TempDir()
	bin, helper := filepath.Join(dir, "proxy.test"), filepath.Join(dir, "wardyn-git-helper")
	env := append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	for _, build := range []*exec.Cmd{
		exec.Command("go", "test", "-c", "-tags", "docker", "-o", bin, "."),
		exec.Command("go", "build", "-o", helper, "../../../cmd/wardyn-git-helper"),
	} {
		build.Env = env
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", build, err, out)
		}
	}
	out, err := exec.Command("docker", "run", "--rm", "--network", "none",
		"-v", bin+":/opt/wardyn-test/proxy.test:ro", "-v", helper+":/opt/wardyn-test/wardyn-git-helper:ro",
		"-e", sshAgentInContainerEnv+"=1", "--entrypoint", "/opt/wardyn-test/proxy.test", sshAgentImage,
		"-test.run", "^TestSSHAgent_DockerClone$", "-test.v", "-test.count=1").CombinedOutput()
	t.Logf("in-container run:\n%s", out)
	if err != nil {
		t.Fatalf("in-container run: %v", err)
	}
	if !strings.Contains(string(out), sshAgentCloneMarker) {
		t.Fatal("the in-container half never reached its clone check")
	}
}

func sshAgentCloneInContainer(t *testing.T) {
	const helper = "/opt/wardyn-test/wardyn-git-helper"
	for _, b := range []string{"ssh", "git", "git-upload-pack", helper} {
		if _, err := exec.LookPath(b); err != nil {
			t.Fatalf("%s not in %s: %v", b, sshAgentImage, err)
		}
	}
	work := t.TempDir()
	home := filepath.Join(work, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	repo := sshCloneSeedRepo(t, work)

	// Two throwaway keys, minted once each by a stand-in control plane.
	edGrant, rsaGrant := uuid.New(), uuid.New()
	keys := map[uuid.UUID]string{}
	pubs := map[string]bool{}
	for grant, rsaKey := range map[uuid.UUID]bool{edGrant: false, rsaGrant: true} {
		pemKey, pub := testSSHKey(t, rsaKey)
		keys[grant], pubs[string(pub.Marshal())] = pemKey, true
	}
	_, hostPriv, hostPub := sshHostKey(t)
	sshAddr := sshServeGit(t, hostPriv, pubs, repo)
	knownHostsLine := knownhosts.Line([]string{knownhosts.Normalize(sshAddr)}, hostPub)
	var mints atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mints.Add(1)
		var req struct {
			GrantID uuid.UUID `json:"grant_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]string{"kind": "ssh_key", "token": keys[req.GrantID], "username": "git", "known_hosts": knownHostsLine + "\n"})
	}))
	t.Cleanup(cp.Close)
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		Sink:            &decisionSink{out: io.Discard, ch: make(chan egress.DecisionLog, 256)},
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(cp)),
		ControlPlaneURL: "http://wardynd.test:8080",
		RunToken:        newTokenSource("RUNTOK"),
	})
	proxySrv := httptest.NewServer(p)
	t.Cleanup(proxySrv.Close)
	t.Setenv("WARDYN_PROXY_URL", proxySrv.URL)

	host, port, _ := net.SplitHostPort(sshAddr)
	clone := func(grant uuid.UUID, dest string) ([]byte, error) {
		cfg := filepath.Join(home, "ssh_config")
		stanza := strings.Join([]string{
			"Host forge", "    HostName " + host, "    Port " + port, "    User git",
			"    IdentityAgent ${WARDYN_SSH_AGENT_DIR}/forge.sock",
			"    StrictHostKeyChecking yes", "    UserKnownHostsFile /dev/null", "    GlobalKnownHostsFile /dev/null",
			"    KnownHostsCommand " + helper + " ssh-known-hosts " + grant.String(), "    BatchMode yes", "",
		}, "\n")
		if err := os.WriteFile(cfg, []byte(stanza), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("git", "clone", "forge:"+repo, dest)
		cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+helper+" ssh-exec --agent forge="+grant.String()+" -- ssh -F "+cfg)
		return cmd.CombinedOutput()
	}
	for _, grant := range []uuid.UUID{edGrant, rsaGrant} {
		dest := filepath.Join(work, "clone-"+grant.String())
		if out, err := clone(grant, dest); err != nil {
			t.Fatalf("clone over ssh (grant %s): %v\n%s", grant, err, out)
		}
		if got, _ := os.ReadFile(filepath.Join(dest, "README")); string(got) != "cloned over ssh\n" {
			t.Fatalf("cloned README = %q", got)
		}
	}
	if n := mints.Load(); n != 2 {
		t.Fatalf("control-plane mints = %d, want one per grant", n)
	}

	// No key on disk anywhere the sandbox user can write, and no agent
	// directory left behind.
	for _, root := range []string{home, os.TempDir(), "/var/tmp", "/dev/shm", "/run"} {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && strings.HasPrefix(d.Name(), "wardyn-ssh-agent-") {
				t.Errorf("agent directory %s outlived its ssh", path)
			}
			if !d.Type().IsRegular() {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			for _, k := range keys {
				body := strings.Split(k, "\n")[2] // a line of the key's own base64
				if bytes.Contains(b, []byte("PRIVATE KEY")) || bytes.Contains(b, []byte(body)) {
					t.Errorf("%s holds private key material", path)
				}
			}
			return nil
		})
	}

	// A sandbox process asking the raw mint route gets no key.
	resp, err := http.Post(proxySrv.URL+routeMint, "application/json", strings.NewReader(`{"grant_id":"`+edGrant.String()+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || bytes.Contains(raw, []byte("PRIVATE KEY")) {
		t.Fatalf("raw mint: %d %q, want a 403 with no key", resp.StatusCode, raw)
	}

	// Closed: nothing signs in any more.
	if resp, err := http.Post(proxySrv.URL+routeSSHAgentClose, "", nil); err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("close: %v %v", resp, err)
	}
	if out, err := clone(edGrant, filepath.Join(work, "after-close")); err == nil {
		t.Fatalf("clone after close succeeded:\n%s", out)
	}
	fmt.Println(sshAgentCloneMarker)
}

// sshCloneSeedRepo is a bare repository with one commit.
func sshCloneSeedRepo(t *testing.T, work string) string {
	t.Helper()
	repo, seed := filepath.Join(work, "repo.git"), filepath.Join(work, "seed")
	steps := [][]string{
		{"init", "-q", "--bare", repo},
		{"init", "-q", seed},
		{"-C", seed, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "--allow-empty", "-m", "seed"},
	}
	for _, s := range steps {
		if out, err := exec.Command("git", s...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", s, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("cloned over ssh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, s := range [][]string{
		{"-C", seed, "add", "README"},
		{"-C", seed, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "-m", "readme"},
		{"-C", seed, "push", "-q", repo, "HEAD:refs/heads/main"},
		{"-C", repo, "symbolic-ref", "HEAD", "refs/heads/main"},
	} {
		if out, err := exec.Command("git", s...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", s, err, out)
		}
	}
	return repo
}

func sshHostKey(t *testing.T) (string, ssh.Signer, ssh.PublicKey) {
	t.Helper()
	pemKey, _ := testSSHKey(t, false)
	signer, err := ssh.ParsePrivateKey([]byte(pemKey))
	if err != nil {
		t.Fatal(err)
	}
	return pemKey, signer, signer.PublicKey()
}

// sshServeGit is a git-over-SSH server on loopback that admits user git with
// one of pubs and runs git-upload-pack for repo only.
func sshServeGit(t *testing.T, host ssh.Signer, pubs map[string]bool, repo string) string {
	t.Helper()
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if c.User() == "git" && pubs[string(k.Marshal())] {
			return nil, nil
		}
		return nil, errors.New("unknown key")
	}}
	cfg.AddHostKey(host)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					if nc.ChannelType() != "session" {
						_ = nc.Reject(ssh.UnknownChannelType, "session only")
						continue
					}
					ch, chReqs, err := nc.Accept()
					if err != nil {
						continue
					}
					go sshServeSession(ch, chReqs, repo)
				}
			}()
		}
	}()
	return l.Addr().String()
}

func sshServeSession(ch ssh.Channel, reqs <-chan *ssh.Request, repo string) {
	for req := range reqs {
		if req.Type != "exec" {
			_ = req.Reply(req.Type == "env", nil)
			continue
		}
		var p struct{ Cmd string }
		if ssh.Unmarshal(req.Payload, &p) != nil || p.Cmd != "git-upload-pack '"+repo+"'" {
			_ = req.Reply(false, nil)
			continue
		}
		_ = req.Reply(true, nil)
		go func() {
			defer func() { _ = ch.Close() }()
			cmd := exec.Command("git-upload-pack", repo)
			cmd.Stdout, cmd.Stderr = ch, ch.Stderr()
			in, err := cmd.StdinPipe()
			if err == nil {
				err = cmd.Start()
			}
			status := uint32(1)
			if err == nil {
				go func() { _, _ = io.Copy(in, ch); _ = in.Close() }()
				if cmd.Wait() == nil {
					status = 0
				}
			}
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		}()
	}
}
