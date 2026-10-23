// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/client"
)

// updateUnsetGolden rewrites testdata/run-network-unset.golden; it was captured from origin/main's
// driver (the lane's BASE overlay), before the pool existed.
var updateUnsetGolden = flag.Bool("update-unset-golden", false, "rewrite testdata/run-network-unset.golden")

// unsetPoolGolden is a sandbox create's network calls with WARDYN_DOCKER_RUN_SUBNET_POOL unset, as
// they were before the pool existed: a daemon-picked probe network, then the run network asking for
// the subnet the probe got.
var unsetPoolGolden = []string{
	"wardyn-int-11111111-1111-1111-1111-111111111111 ipam=none internal=true driver=bridge labels=map[wardyn.component:network wardyn.deployment:wardyn-internal wardyn.managed:true wardyn.run-id:11111111-1111-1111-1111-111111111111]",
	"wardyn-int-11111111-1111-1111-1111-111111111111 ipam=10.88.0.0/16 internal=true driver=bridge labels=map[wardyn.component:network wardyn.deployment:wardyn-internal wardyn.managed:true wardyn.run-id:11111111-1111-1111-1111-111111111111]",
}

// TestRunSubnetPool_UnsetIsUnchanged: with the pool unset a sandbox's network calls match the golden,
// and neither the create nor the orphan sweep lists the daemon's networks.
func TestRunSubnetPool_UnsetIsUnchanged(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	var creates []string
	f.onNetworkCreate = func(name string, opts client.NetworkCreateOptions) {
		ipam := "none"
		if opts.IPAM != nil {
			ipam = fmt.Sprint(opts.IPAM.Config[0].Subnet)
		}
		creates = append(creates, fmt.Sprintf("%s ipam=%s internal=%t driver=%s labels=%v", name, ipam, opts.Internal, opts.Driver, opts.Labels))
	}
	d := newTestDriver(f)
	if _, err := d.CreateSandbox(context.Background(), testSpec()); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if !slices.Equal(creates, unsetPoolGolden) {
		t.Fatalf("network creates with the pool unset:\n got %q\nwant %q", creates, unsetPoolGolden)
	}
	if _, err := d.SweepOrphanedSandboxes(context.Background(), time.Minute, func(uuid.UUID) bool { return true }); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if f.networkLists != 0 {
		t.Errorf("%d NetworkList calls with the pool unset; want none", f.networkLists)
	}
}

// TestRunSubnetPool_UnsetSendsMainsRequests: with the pool unset, the requests the real Docker client
// sends to create a run network (probe create, inspect, remove, create) are byte for byte the ones
// origin/main's driver sent, held in testdata/run-network-unset.golden.
func TestRunSubnetPool_UnsetSendsMainsRequests(t *testing.T) {
	var mu sync.Mutex
	var got strings.Builder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.Header().Set("Api-Version", "1.44")
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		fmt.Fprintf(&got, "%s %s?%s\n%s\n", r.Method, r.URL.Path, r.URL.RawQuery, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/networks/create"):
			_, _ = io.WriteString(w, `{"Id":"net1"}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"Id":"net1","IPAM":{"Config":[{"Subnet":"10.88.0.0/16"}]}}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	cli, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(srv.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	d := newWithClient(newEngineClient(cli), Config{ProxyImage: "wardyn-proxy:dev"})
	id := testSpec().RunID
	if _, err := d.createRunNetwork(context.Background(), id, d.labels(id, "network", nil)); err != nil {
		t.Fatalf("createRunNetwork: %v", err)
	}
	const golden = "testdata/run-network-unset.golden"
	if *updateUnsetGolden {
		if err := os.WriteFile(golden, []byte(got.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Fatalf("Docker API requests with the pool unset differ from main's:\n got %s\nwant %s", got.String(), want)
	}
}
