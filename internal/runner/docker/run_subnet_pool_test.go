// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

func newPoolDriver(f *fakeDocker, pool string) *Driver {
	return newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", RunSubnetPool: netip.MustParsePrefix(pool), RunSubnetBits: 28})
}

// poolNet creates a run network for a fresh run id and returns its name and subnet.
func poolNet(t *testing.T, f *fakeDocker, d *Driver) (string, netip.Prefix, error) {
	t.Helper()
	id := uuid.New()
	if _, err := d.createRunNetwork(context.Background(), id, d.labels(id, componentNetwork, nil)); err != nil {
		return "", netip.Prefix{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return internalNetName(id), f.subnets[internalNetName(id)], nil
}

func wantSubnets(t *testing.T, f *fakeDocker, d *Driver, want ...string) {
	t.Helper()
	for _, w := range want {
		_, got, err := poolNet(t, f, d)
		if err != nil || got != netip.MustParsePrefix(w) {
			t.Fatalf("run network subnet = %v, %v; want %s", got, err, w)
		}
	}
}

func TestRunSubnetPool_TakesTheNextFreeSubnet(t *testing.T) {
	f := newFakeDocker()
	wantSubnets(t, f, newPoolDriver(f, "10.200.0.0/24"), "10.200.0.0/28", "10.200.0.16/28", "10.200.0.32/28")
}

func TestRunSubnetPool_ReusesAReleasedSubnet(t *testing.T) {
	f := newFakeDocker()
	d := newPoolDriver(f, "10.200.0.0/24")
	first, _, err := poolNet(t, f, d)
	if err != nil {
		t.Fatal(err)
	}
	wantSubnets(t, f, d, "10.200.0.16/28")
	if _, err := f.NetworkRemove(context.Background(), first, client.NetworkRemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	wantSubnets(t, f, d, "10.200.0.0/28", "10.200.0.32/28")
}

func TestRunSubnetPool_RefusesWhenExhaustedNamingTheSetting(t *testing.T) {
	f := newFakeDocker()
	d := newPoolDriver(f, "10.200.0.0/27")
	wantSubnets(t, f, d, "10.200.0.0/28", "10.200.0.16/28")
	before := len(f.networks)
	_, _, err := poolNet(t, f, d)
	if err == nil || !strings.Contains(err.Error(), "no free /28 subnet is left in "+EnvRunSubnetPool+"=10.200.0.0/27") {
		t.Fatalf("create on a full pool = %v; want the refusal naming %s", err, EnvRunSubnetPool)
	}
	if len(f.networks) != before {
		t.Errorf("a refused create left a network behind: %d networks, want %d", len(f.networks), before)
	}
}

// TestRunSubnetPool_RestartDerivesWhatIsInUse: a new driver (a restarted wardynd, a second replica)
// reads the subnets in use from the daemon's networks, its own and others', and gets a free one
// first time rather than learning by failed creates.
func TestRunSubnetPool_RestartDerivesWhatIsInUse(t *testing.T) {
	f := newFakeDocker()
	before := newPoolDriver(f, "10.200.0.0/24")
	wantSubnets(t, f, before, "10.200.0.0/28", "10.200.0.16/28", "10.200.0.32/28", "10.200.0.48/28")
	f.mu.Lock()
	f.networks["compose_default"] = client.NetworkCreateOptions{}
	f.subnets["compose_default"] = netip.MustParsePrefix("10.200.0.64/27")
	f.mu.Unlock()
	var creates atomic.Int32
	f.onNetworkCreate = func(string, client.NetworkCreateOptions) { creates.Add(1) }
	wantSubnets(t, f, newPoolDriver(f, "10.200.0.0/24"), "10.200.0.96/28")
	if n := creates.Load(); n != 1 {
		t.Errorf("the restarted driver made %d network creates; want 1, the subnet read from the daemon", n)
	}
}

// TestRunSubnetPool_RetriesASubnetAnotherProcessTook: between the list and the create another process
// takes the subnet; the create fails on the overlap and the next free one is taken.
func TestRunSubnetPool_RetriesASubnetAnotherProcessTook(t *testing.T) {
	f := newFakeDocker()
	var once sync.Once
	f.onNetworkCreate = func(_ string, opts client.NetworkCreateOptions) {
		if opts.IPAM == nil {
			return // a daemon-picked create: not the pool's
		}
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.networks["rival"] = client.NetworkCreateOptions{IPAM: opts.IPAM}
			f.subnets["rival"] = opts.IPAM.Config[0].Subnet
		})
	}
	wantSubnets(t, f, newPoolDriver(f, "10.200.0.0/24"), "10.200.0.16/28")
}

// TestRunSubnetPool_ConcurrentCreatesAreSerialised: this driver's creates never pick and create at
// once, so none of them races another for one subnet; run under -race.
func TestRunSubnetPool_ConcurrentCreatesAreSerialised(t *testing.T) {
	f := newFakeDocker()
	d := newPoolDriver(f, "10.200.0.0/24")
	var inFlight, most atomic.Int32
	f.onNetworkCreate = func(string, client.NetworkCreateOptions) {
		n := inFlight.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		time.Sleep(2 * time.Millisecond)
		inFlight.Add(-1)
	}
	const runs = 12
	var wg sync.WaitGroup
	errs := make(chan error, runs)
	for range runs {
		wg.Go(func() {
			if _, _, err := poolNet(t, f, d); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent create: %v", err)
	}
	if m := most.Load(); m != 1 {
		t.Errorf("%d network creates ran at once; want them serialised", m)
	}
	seen := map[netip.Prefix]bool{}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.subnets {
		if seen[s] {
			t.Errorf("subnet %s handed to two run networks", s)
		}
		seen[s] = true
	}
	if len(seen) != runs {
		t.Errorf("%d run networks, want %d", len(seen), runs)
	}
}

// TestCreateSandbox_PassesPoolIPAMOnlyWhenSet: with the pool set, the run network is created once,
// asking for a pool subnet, with no daemon-picked probe network.
func TestCreateSandbox_PassesPoolIPAMOnlyWhenSet(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	var creates []string
	f.onNetworkCreate = func(name string, opts client.NetworkCreateOptions) {
		creates = append(creates, fmt.Sprintf("%s ipam=%v", name, opts.IPAM))
	}
	if _, err := newPoolDriver(f, "10.200.0.0/24").CreateSandbox(context.Background(), testSpec()); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	name := internalNetName(testSpec().RunID)
	want := fmt.Sprintf("%s ipam=%v", name, &network.IPAM{Config: []network.IPAMConfig{{Subnet: netip.MustParsePrefix("10.200.0.0/28")}}})
	if len(creates) != 1 || creates[0] != want {
		t.Fatalf("network creates = %q; want one: %q", creates, want)
	}
	if opts := f.networks[name]; !opts.Internal || opts.Driver != "bridge" || opts.Labels[labelComponent] != componentNetwork {
		t.Errorf("run network = %+v; want an internal bridge labelled as the run's network", opts)
	}
}

// TestSweepOrphanedSandboxes_ReleasesOrphanRunNetworks: with the pool set, a network left with no
// container (a crash between network create and proxy start) is removed once its run is gone, so its
// subnet goes back to the pool; a live run's, a young one and another deployment's stay.
func TestSweepOrphanedSandboxes_ReleasesOrphanRunNetworks(t *testing.T) {
	f := newFakeDocker()
	d := newPoolDriver(f, "10.200.0.0/24")
	orphan, live, young, other := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	ctx := context.Background()
	for _, id := range []uuid.UUID{orphan, live, young} {
		if _, err := d.createRunNetwork(ctx, id, d.labels(id, componentNetwork, nil)); err != nil {
			t.Fatal(err)
		}
	}
	otherLabels := d.labels(other, componentNetwork, nil)
	otherLabels[labelDeployment] = "wardyn-internal-other"
	if _, err := d.createRunNetwork(ctx, other, otherLabels); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	for _, id := range []uuid.UUID{orphan, live, other} {
		f.netCreated[internalNetName(id)] = time.Now().Add(-time.Hour)
	}
	f.mu.Unlock()
	n, err := d.SweepOrphanedSandboxes(ctx, 10*time.Minute, func(id uuid.UUID) bool { return id != live })
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v; want 1 network swept", n, err)
	}
	if _, ok := f.networks[internalNetName(orphan)]; ok {
		t.Error("the orphaned run's network is still there")
	}
	for _, id := range []uuid.UUID{live, young, other} {
		if _, ok := f.networks[internalNetName(id)]; !ok {
			t.Errorf("network %s was swept; want it kept", internalNetName(id))
		}
	}
	wantSubnets(t, f, d, "10.200.0.0/28")
}

func TestRunSubnetPoolFromEnv(t *testing.T) {
	env := func(pool, size string) func(string) string {
		return func(k string) string { return map[string]string{EnvRunSubnetPool: pool, EnvRunSubnetSize: size}[k] }
	}
	if p, b, err := RunSubnetPoolFromEnv(env("", "")); err != nil || p.IsValid() || b != 0 {
		t.Errorf("unset = %v /%d %v; want the zero prefix", p, b, err)
	}
	if p, b, err := RunSubnetPoolFromEnv(env("10.200.0.0/16", "")); err != nil || p != netip.MustParsePrefix("10.200.0.0/16") || b != 28 {
		t.Errorf("pool alone = %v /%d %v; want 10.200.0.0/16 in /28s", p, b, err)
	}
	if _, b, err := RunSubnetPoolFromEnv(env("10.200.0.0/16", "26")); err != nil || b != 26 {
		t.Errorf("size 26 = /%d %v", b, err)
	}
	for _, c := range [][2]string{{"10.200.0.1/16", ""}, {"fd00::/64", ""}, {"nonsense", ""}, {"10.200.0.0/16", "30"},
		{"10.200.0.0/24", "20"}, {"10.200.0.0/16", "x"}, {"", "28"}} {
		if _, _, err := RunSubnetPoolFromEnv(env(c[0], c[1])); err == nil || !strings.Contains(err.Error(), "WARDYN_DOCKER_RUN_SUBNET_") {
			t.Errorf("pool %q size %q = %v; want a refusal naming the setting", c[0], c[1], err)
		}
	}
}
