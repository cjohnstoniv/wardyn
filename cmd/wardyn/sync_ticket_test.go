// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// `wardyn sync` on the ticket path: its own in-process client over GET /api/v1/runs/{id}/ssh.

// syncTicketFixture is a deployment with SSH tickets on: /healthz, the run (its sync rules), the mint and the ticket
// door, an in-process SSH server that takes the "none" method, as the real door does after a redeemed ticket, and
// opens no channel, so a sync that reaches it ends at "open a channel".
type syncTicketFixture struct {
	url                      string
	mints, dials, handshakes atomic.Int32
	mu                       sync.Mutex
	mintQ                    []string
	doorTicket, doorQuery    []string
	ticketInEnv              bool // the ticket was in os.Environ() while the door answered
	mint                     func(w http.ResponseWriter, q url.Values)
}

// newSyncTicketFixture serves healthz(door's host key) on /healthz.
func newSyncTicketFixture(t *testing.T, healthz func(testHostKey) string) *syncTicketFixture {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pub := signer.PublicKey()
	hk := testHostKey{key: pub, line: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), print: ssh.FingerprintSHA256(pub)}
	f := &syncTicketFixture{}
	f.mint = func(w http.ResponseWriter, q url.Values) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ticket":"tkt-%d","expires_in_seconds":30,"purpose":%q}`, f.mints.Load(), q.Get("purpose"))
	}
	body := healthz(hk)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
	mux.HandleFunc("GET /api/v1/runs/"+testRunID, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"sync":{}}`)) })
	mux.HandleFunc("POST /api/v1/runs/"+testRunID+"/attach/ticket", func(w http.ResponseWriter, r *http.Request) {
		f.mints.Add(1)
		f.mu.Lock()
		f.mintQ = append(f.mintQ, r.URL.Query().Encode())
		f.mu.Unlock()
		f.mint(w, r.URL.Query())
	})
	mux.HandleFunc("/api/v1/runs/"+testRunID+"/ssh", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		f.mu.Lock()
		f.doorTicket = append(f.doorTicket, r.Header.Get("Wardyn-Ticket"))
		f.doorQuery = append(f.doorQuery, r.URL.RawQuery)
		for _, kv := range os.Environ() {
			f.ticketInEnv = f.ticketInEnv || strings.Contains(kv, "tkt-")
		}
		f.mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		nc := websocket.NetConn(r.Context(), c, websocket.MessageBinary)
		defer nc.Close()
		cfg := &ssh.ServerConfig{NoClientAuth: true}
		cfg.AddHostKey(signer)
		sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
		if err != nil {
			return
		}
		defer sc.Close()
		f.handshakes.Add(1)
		go ssh.DiscardRequests(reqs)
		for ch := range chans {
			_ = ch.Reject(ssh.Prohibited, "this test door opens nothing")
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// syncHealthz is /healthz for the sync fixture: tickets on, the door's host key, and extra fields.
func syncHealthz(extra string) func(testHostKey) string {
	return func(hk testHostKey) string { return ticketHealthz(hk, true, extra) }
}

// runTicketSync runs `wardyn sync <run> <dir> flags...` against f, with a two-file folder.
func runTicketSync(t *testing.T, f *syncTicketFixture, flags ...string) error {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "a")
	writeFile(t, filepath.Join(dir, "b.txt"), "b")
	_, _, err := runTicketCmd(t, syncCmd, f.url, "bearer-x", strings.NewReader(""), append([]string{testRunID, dir}, flags...)...)
	return err
}

// reachedTheDoor is the error a sync gets once its handshake with the test door succeeded.
func reachedTheDoor(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "open a channel") {
		t.Fatalf("error = %v, want the handshake done and the test door's channel refusal", err)
	}
}

func TestSyncTicket_DialCarriesTheHeaderAndNoQuery(t *testing.T) {
	homeWithKey(t, false)
	f := newSyncTicketFixture(t, syncHealthz(""))
	reachedTheDoor(t, runTicketSync(t, f))
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.doorTicket) != 1 || f.doorTicket[0] != "tkt-1" || f.doorQuery[0] != "" || f.handshakes.Load() != 1 {
		t.Errorf("door saw ticket %q, query %q, handshakes %d; want the minted ticket in the header and no query",
			f.doorTicket, f.doorQuery, f.handshakes.Load())
	}
	if f.ticketInEnv || os.Getenv(sshTicketEnv) != "" {
		t.Error("the ticket reached this process's environment")
	}
}

// TestSyncTicket_PurposeByFlags: --git and --pull-commits both run git over exec, so either takes ssh-sync-git.
func TestSyncTicket_PurposeByFlags(t *testing.T) {
	for _, tc := range []struct {
		flags []string
		want  string
	}{
		{nil, "purpose=ssh-sync"},
		{[]string{"--git"}, "purpose=ssh-sync-git"},
		{[]string{"--pull-commits"}, "purpose=ssh-sync-git"},
		{[]string{"--git", "--pull-commits"}, "purpose=ssh-sync-git"},
	} {
		t.Run(strings.Join(append([]string{"plain"}, tc.flags...), " "), func(t *testing.T) {
			homeWithKey(t, false)
			f := newSyncTicketFixture(t, syncHealthz(""))
			reachedTheDoor(t, runTicketSync(t, f, tc.flags...))
			if len(f.mintQ) != 1 || f.mintQ[0] != tc.want {
				t.Errorf("mints = %q, want one %q", f.mintQ, tc.want)
			}
		})
	}
}

func TestSyncTicket_EchoAndHostKeyMismatchRefused(t *testing.T) {
	t.Run("no echo", func(t *testing.T) {
		homeWithKey(t, false)
		f := newSyncTicketFixture(t, syncHealthz(""))
		f.mint = func(w http.ResponseWriter, _ url.Values) { _, _ = w.Write([]byte(`{"ticket":"terminal-ticket"}`)) }
		err := runTicketSync(t, f)
		if want := "sync: this server minted a ticket that does not open SSH; run again with --key"; err == nil || err.Error() != want || f.dials.Load() != 0 {
			t.Errorf("error = %v, dials %d; want %q and no dial", err, f.dials.Load(), want)
		}
	})
	t.Run("host key", func(t *testing.T) {
		homeWithKey(t, false)
		other := newTestHostKey(t)
		f := newSyncTicketFixture(t, func(testHostKey) string { return ticketHealthz(other, true, "") })
		err := runTicketSync(t, f)
		if err == nil || !strings.Contains(err.Error(), "is not the advertised "+other.print) || f.handshakes.Load() != 0 {
			t.Errorf("error = %v, handshakes %d; want the host key refused", err, f.handshakes.Load())
		}
	})
}

func TestSyncTicket_KeyTakesTheKeyPath(t *testing.T) {
	home := homeWithKey(t, true)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, ".wardyn", "id_ed25519"), string(pem.EncodeToMemory(block)))
	addr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	f := newSyncTicketFixture(t, func(hk testHostKey) string {
		return `{"ssh":{"enabled":true,"advertise_addr":"` + addr + `","host_key_fingerprint":"` + hk.print + `","tickets":true,"host_key":"` + hk.line + `"}}`
	})
	err = runTicketSync(t, f, "--key")
	if err == nil || !strings.Contains(err.Error(), "sync: connect to the gateway") || f.mints.Load() != 0 || f.dials.Load() != 0 {
		t.Errorf("error = %v, mints %d, dials %d; want the key path's TCP dial and no ticket", err, f.mints.Load(), f.dials.Load())
	}
}

// TestSyncTicket_AdvertisedProxyNeedsNoConsentButAFingerprint: the ticket path skips only the ProxyCommand
// consent; a gateway with no fingerprint is still refused.
func TestSyncTicket_AdvertisedProxyNeedsNoConsentButAFingerprint(t *testing.T) {
	t.Run("advertised ProxyCommand", func(t *testing.T) {
		homeWithKey(t, false)
		f := newSyncTicketFixture(t, syncHealthz(`,"proxy_command":"nc %h %p"`))
		reachedTheDoor(t, runTicketSync(t, f))
		if f.dials.Load() != 1 {
			t.Errorf("dials = %d, want the ticket door", f.dials.Load())
		}
	})
	t.Run("no fingerprint", func(t *testing.T) {
		homeWithKey(t, false)
		f := newSyncTicketFixture(t, func(hk testHostKey) string {
			return `{"ssh":{"enabled":true,"advertise_addr":"gw.example.com:2222","host_key_fingerprint":"","tickets":true,"host_key":"` + hk.line + `"}}`
		})
		err := runTicketSync(t, f)
		if err == nil || !strings.Contains(err.Error(), "publishes no host key fingerprint") || f.mints.Load() != 0 {
			t.Errorf("error = %v, mints %d; want the fingerprint refusal before any mint", err, f.mints.Load())
		}
	})
}

// TestSyncTicket_GitWithExecOff: with exec off, --git and --pull-commits get one line that does not point to --key,
// whose path needs exec for git too.
func TestSyncTicket_GitWithExecOff(t *testing.T) {
	const want = "sync: this deployment's SSH gateway does not serve exec (WARDYN_SSH_CHANNELS), which --git and --pull-commits run over; run again without them"
	for _, flag := range []string{"--git", "--pull-commits"} {
		t.Run(flag, func(t *testing.T) {
			homeWithKey(t, false)
			f := newSyncTicketFixture(t, syncHealthz(""))
			f.mint = func(w http.ResponseWriter, _ url.Values) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"This deployment's SSH gateway does not serve commands (exec) (WARDYN_SSH_CHANNELS).","reason":"ssh_ticket_channel_off"}`))
			}
			err := runTicketSync(t, f, flag)
			if err == nil || err.Error() != want || exitCodeFor(err) != 1 || f.dials.Load() != 0 {
				t.Errorf("error = %v (exit %d), dials %d; want %q, exit 1, no dial", err, exitCodeFor(err), f.dials.Load(), want)
			}
		})
	}
}
