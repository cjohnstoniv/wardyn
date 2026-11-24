// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// outageDoor is a decision door that a test can take away: while down it reads a post, notes the ids it
// carried, and closes the connection with no answer. Up, it answers every post 202 with the batch advertisement
// and notes which ids landed.
type outageDoor struct {
	mu      sync.Mutex
	down    bool
	seen    map[string]string // id -> host, every id a post carried
	landed  map[string]int    // id -> the 2xx answers it got
	replays int               // posts that carried the replay mark
	dropped int               // synthetic dropped-decision summaries received
}

func (o *outageDoor) setDown(down bool) {
	o.mu.Lock()
	o.down = down
	o.mu.Unlock()
}

func (o *outageDoor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	var batch struct {
		Records []struct {
			ID       string `json:"id"`
			Decision struct {
				Request struct {
					Host string `json:"Host"`
				} `json:"request"`
				RuleSource string `json:"rule_source"`
			} `json:"decision"`
		} `json:"records"`
	}
	_ = json.Unmarshal(body, &batch)
	o.mu.Lock()
	defer o.mu.Unlock()
	if r.Header.Get("Wardyn-Decision-Replay") == "1" {
		o.replays++
	}
	if strings.Contains(string(body), "egress:dropped-decisions-") {
		o.dropped++
	}
	for _, rec := range batch.Records {
		o.seen[rec.ID] = rec.Decision.Request.Host
	}
	if o.down {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
		return
	}
	for _, rec := range batch.Records {
		o.landed[rec.ID]++
	}
	w.Header().Set("Wardyn-Decision-Batch", "128")
	w.WriteHeader(http.StatusAccepted)
}

// liveOutageDoor serves door where a run's proxy reaches this host, as liveRenewingControlPlane does, and
// returns its URL and the CA to pin.
func liveOutageDoor(ctx context.Context, t *testing.T, d *Driver, door *outageDoor) (url, caPEM string) {
	t.Helper()
	blob, err := hoptls.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := hoptls.ParseCA(blob)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.ServingCert("host.docker.internal", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	bind := netip.MustParseAddr("127.0.0.1")
	if info, err := d.cli.Info(ctx, client.InfoOptions{}); err != nil {
		t.Fatal(err)
	} else if info.Info.OperatingSystem != "Docker Desktop" {
		insp, err := d.cli.NetworkInspect(ctx, "bridge", client.NetworkInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		bind = netip.Addr{}
		for _, c := range insp.Network.IPAM.Config {
			if c.Gateway.Is4() {
				bind = c.Gateway
			}
		}
		if !bind.IsValid() {
			t.Fatal("the default bridge has no IPv4 gateway")
		}
	}
	ln, err := net.Listen("tcp", netip.AddrPortFrom(bind, 0).String())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/internal/token/renew", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	mux.Handle("POST /api/v1/internal/decisions", door)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	return fmt.Sprintf("https://host.docker.internal:%d", ln.Addr().(*net.TCPAddr).Port), string(ca.CertPEM)
}

// TestDecisionSpool_RealProxySurvivesOutage: a real server proxy container whose control plane vanishes for
// 60 seconds keeps the decisions it makes meanwhile and sends them when the control plane is back. Every decision
// made in the outage lands, once or more under its own id, and the proxy reports no drop.
func TestDecisionSpool_RealProxySurvivesOutage(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the live decision spool test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	d, err := New(Config{ProxyImage: liveProxyImage})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	ensureNetwork(t, d, d.cfg.InternalNetwork)
	door := &outageDoor{seen: map[string]string{}, landed: map[string]int{}}
	cpURL, cpCA := liveOutageDoor(ctx, t, d, door)
	spec := runner.SandboxSpec{
		RunID:            uuid.New(),
		Image:            "busybox:latest",
		ConfinementClass: types.CC1,
		ProxyConfig: runner.ProxyConfig{RunToken: "spool-live-test", ControlPlaneURL: cpURL, ControlPlaneCAPEM: cpCA,
			Policy: types.RunPolicySpec{AllowedDomains: []string{"allowed.example.com"}}},
		Resources: runner.Resources{CPUMillis: 500, MemoryMiB: 128},
	}
	sb, err := d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() { _ = d.KillSandbox(context.Background(), sb.Ref) })
	waitForLog(ctx, t, proxyContainerName(spec.RunID), "listening")

	// Every request below is refused by the proxy (its host is not allowed), which is a decision made with no
	// network: one row each, with the host in it.
	proxyURL := fmt.Sprintf("http://wardyn-proxy:%d", runner.ProxyListenPort)
	request := func(host string) {
		execInSandbox(ctx, t, d, sb.Ref, []string{"sh", "-c", "http_proxy=" + proxyURL + " wget -q -O /dev/null -T 3 http://" + host + "/ 2>&1; true"})
	}
	for i := range 3 { // the control plane is up: the proxy learns that it takes batches
		request(fmt.Sprintf("warm-%d.invalid", i))
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		door.mu.Lock()
		n := len(door.landed)
		door.mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	door.setDown(true)
	var made []string
	outageEnds := time.Now().Add(60 * time.Second)
	for i := 0; time.Now().Before(outageEnds); i++ {
		host := fmt.Sprintf("outage-%d.invalid", i)
		made = append(made, host)
		request(host)
		time.Sleep(4 * time.Second)
	}
	door.setDown(false)

	want := map[string]bool{}
	for _, h := range made {
		want[h] = false
	}
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		door.mu.Lock()
		for id, host := range door.seen {
			if _, ok := want[host]; ok && door.landed[id] > 0 {
				want[host] = true
			}
		}
		door.mu.Unlock()
		all := true
		for _, ok := range want {
			all = all && ok
		}
		if all {
			break
		}
	}
	door.mu.Lock()
	defer door.mu.Unlock()
	for host, ok := range want {
		if !ok {
			t.Errorf("the decision for %s made in the outage never landed", host)
		}
	}
	if door.replays == 0 {
		t.Error("no post carried the replay mark: the decisions did not come from the spool")
	}
	if door.dropped != 0 {
		t.Errorf("the proxy reported %d drop summaries, want none", door.dropped)
	}
}
