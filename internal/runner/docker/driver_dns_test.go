// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// dnsTestDriver is a driver on a fake daemon that has the busybox image.
func dnsTestDriver(cfg Config) (*Driver, *fakeDocker) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	cfg.ProxyImage = "wardyn-proxy:dev"
	return newWithClient(f, cfg), f
}

// resolverSummary is the mountSummary of runID's resolver mount, for the tests that assert an agent's whole mount set.
func resolverSummary(runID uuid.UUID) mountSummary {
	return mountSummary{Target: "/etc/resolv.conf", Source: dnsVolumeName(runID), ReadOnly: true}
}

// proxyIPOf is the proxy address the agent's hosts entry pins.
func proxyIPOf(t *testing.T, agent *createdContainer) string {
	t.Helper()
	for _, h := range agent.host.ExtraHosts {
		if ip, ok := strings.CutPrefix(h, "wardyn-proxy:"); ok {
			return ip
		}
	}
	t.Fatalf("no wardyn-proxy hosts entry in %v", agent.host.ExtraHosts)
	return ""
}

func dnsSeedCopies(f *fakeDocker, runID uuid.UUID) []fakeCopy {
	var out []fakeCopy
	for _, c := range f.copies {
		if c.id == dnsSeedName(runID) {
			out = append(out, c)
		}
	}
	return out
}

func foreignDNSVolume(d *Driver, runID uuid.UUID, mutate func(*client.VolumeCreateOptions)) client.VolumeCreateOptions {
	opts := client.VolumeCreateOptions{Name: dnsVolumeName(runID), Driver: driveVolumeDriver, Labels: d.dnsLabels(runID, componentDNSVolume)}
	opts.Labels = maps.Clone(opts.Labels)
	mutate(&opts)
	return opts
}

// TestCreateSandbox_AgentResolverIsTheProxy: the agent mounts a per-run volume's resolv.conf read-only over
// /etc/resolv.conf, the file names the proxy's address and nothing else, the seed that wrote it is gone and never
// ran, and the proxy is told to answer on :53.
func TestCreateSandbox_AgentResolverIsTheProxy(t *testing.T) {
	d, f := dnsTestDriver(Config{})
	spec := testSpec()
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	agent := f.containers[agentContainerName(spec.RunID)]
	m := findMount(agent.host.Mounts, "/etc/resolv.conf")
	if m == nil || m.Type != mount.TypeVolume || m.Source != dnsVolumeName(spec.RunID) || !m.ReadOnly ||
		m.VolumeOptions == nil || !m.VolumeOptions.NoCopy || m.VolumeOptions.Subpath != "resolv.conf" {
		t.Fatalf("resolver mount = %+v, want a read-only NoCopy volume mount of subpath resolv.conf", m)
	}
	opts, ok := f.volumes[dnsVolumeName(spec.RunID)]
	if !ok || opts.Driver != driveVolumeDriver || len(opts.DriverOpts) != 0 ||
		opts.Labels[labelComponent] != componentDNSVolume || opts.Labels[labelRun] != spec.RunID.String() {
		t.Fatalf("DNS volume = %+v (present %v), want the local driver, no options, this run's DNS labels", opts, ok)
	}
	seed := f.containers[dnsSeedName(spec.RunID)]
	if seed == nil || !seed.removed || slices.Contains(f.startedNames, dnsSeedName(spec.RunID)) {
		t.Fatalf("seed = %+v, want one created and removed, never started", seed)
	}
	if seed.cfg.Image != "wardyn-proxy:dev" || string(seed.host.NetworkMode) != "none" || !seed.cfg.NetworkDisabled ||
		len(seed.host.Mounts) != 1 || seed.host.Mounts[0].Source != dnsVolumeName(spec.RunID) ||
		seed.host.Mounts[0].Target != dnsSeedTarget || seed.host.Mounts[0].VolumeOptions == nil || !seed.host.Mounts[0].VolumeOptions.NoCopy {
		t.Errorf("seed config = %+v host %+v, want the proxy image, no network, the volume mounted NoCopy at %s", seed.cfg, seed.host, dnsSeedTarget)
	}
	copies := dnsSeedCopies(f, spec.RunID)
	if len(copies) != 1 || copies[0].dest != "/" || copies[0].copyUIDGID || copies[0].afterStart {
		t.Fatalf("seed copies = %+v, want one archive at / with CopyUIDGID false", copies)
	}
	tr := tar.NewReader(bytes.NewReader(copies[0].archive))
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(tr)
	if hdr.Name != "wardyn-dns-seed/resolv.conf" || hdr.Mode != 0o644 || hdr.Uid != 0 || hdr.Gid != 0 || hdr.Typeflag != tar.TypeReg {
		t.Errorf("archive entry = %+v, want wardyn-dns-seed/resolv.conf, mode 0644, root", hdr)
	}
	if want := "nameserver " + proxyIPOf(t, agent) + "\n"; string(body) != want {
		t.Errorf("resolv.conf = %q, want %q", body, want)
	}
	if _, err := tr.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("archive holds more than the one file (%v)", err)
	}
	p := f.containers[proxyContainerName(spec.RunID)]
	if !slices.Contains(p.cfg.Env, proxy.DNSListenEnv+"=:53") {
		t.Errorf("proxy env = %v, want %s=:53", p.cfg.Env, proxy.DNSListenEnv)
	}
}

// TestCreateSandbox_ExecLessAgentCarriesTheResolver: the krun path stashes the agent's config until Exec, and the
// resolver mount is in what it stashed.
func TestCreateSandbox_ExecLessAgentCarriesTheResolver(t *testing.T) {
	d, f := dnsTestDriver(Config{})
	f.info = infoWithRuntimes("krun")
	spec := testSpec()
	spec.ConfinementClass = types.CC3
	ctx := context.Background()
	sb, err := d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if _, err := d.Exec(ctx, sb.Ref, []string{"agent-run", "task"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if m := findMount(f.containers[sb.Ref].host.Mounts, "/etc/resolv.conf"); m == nil || m.Source != dnsVolumeName(spec.RunID) || !m.ReadOnly {
		t.Errorf("exec-less agent resolver mount = %+v, want the run's DNS volume read-only", m)
	}
}

// TestCreateSandbox_ResolverNeedsEngineAndAPI lists the daemon and API versions the mount is offered for. Under
// WARDYN_ALLOW_HOST_REACHABLE_RUN_NETWORK an Engine older than 28 runs; DOCKER_API_VERSION can pin the client below
// a new Engine's API.
func TestCreateSandbox_ResolverNeedsEngineAndAPI(t *testing.T) {
	cases := []struct {
		name, engine, api string
		want              bool
	}{
		{"engine 29, API 1.56", "29.4.2", "1.56", true},
		{"engine 28, API 1.45", "28.0.0", "1.45", true},
		{"engine 26, API 1.45", "26.0.0", "1.45", true},
		{"engine 25, API 1.44", "25.0.0", "1.44", false},
		{"engine 29, API pinned to 1.44", "29.4.2", "1.44", false},
		{"engine 29, API pinned to 1.40", "29.4.2", "1.40", false},
		{"engine 25, API 1.56", "25.0.0", "1.56", false},
		{"engine 29, API unknown", "29.4.2", "", false},
		{"engine 29, API unreadable", "29.4.2", "latest", false},
		{"engine unreadable, API 1.56", "dev", "1.56", false},
		{"API 1.100 is later than 1.45", "29.4.2", "1.100", true},
		{"API 2.0", "29.4.2", "2.0", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, f := dnsTestDriver(Config{AllowHostReachableRunNetwork: true})
			f.info.ServerVersion = tc.engine
			f.apiVersion, f.apiVersionSet = tc.api, true
			spec := testSpec()
			if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}
			agent := f.containers[agentContainerName(spec.RunID)]
			mounted := findMount(agent.host.Mounts, "/etc/resolv.conf") != nil
			_, volume := f.volumes[dnsVolumeName(spec.RunID)]
			if mounted != tc.want || volume != tc.want || (len(dnsSeedCopies(f, spec.RunID)) == 1) != tc.want {
				t.Errorf("engine %q API %q: mounted %v, volume %v, seeded %v; want all %v",
					tc.engine, tc.api, mounted, volume, len(dnsSeedCopies(f, spec.RunID)) == 1, tc.want)
			}
		})
	}
}

func TestAPIAtLeast(t *testing.T) {
	for in, want := range map[string]bool{"1.45": true, "v1.45": true, " 1.46 ": true, "1.56": true, "1.100": true, "2.0": true,
		"1.44": false, "1.9": false, "0.99": false, "": false, "1": false, "1.x": false, "latest": false} {
		if got := apiAtLeast(in, 1, 45); got != want {
			t.Errorf("apiAtLeast(%q, 1, 45) = %v, want %v", in, got, want)
		}
	}
}

// TestCreateSandbox_RefusesAForeignDNSVolume: a volume that answers to the run's DNS volume name but is not the
// one this driver makes is refused before anything is written to it, and is left exactly as it was by the
// rollback. The last row is the create race: the first inspect misses the volume, so the create is handed the
// other party's volume.
func TestCreateSandbox_RefusesAForeignDNSVolume(t *testing.T) {
	run := testSpec().RunID
	cases := map[string]func(*client.VolumeCreateOptions){
		"another driver": func(o *client.VolumeCreateOptions) { o.Driver = "nfs" },
		"driver options": func(o *client.VolumeCreateOptions) {
			o.DriverOpts = map[string]string{"type": "cifs", "device": "//host/share"}
		},
		"no labels":          func(o *client.VolumeCreateOptions) { o.Labels = nil },
		"another component":  func(o *client.VolumeCreateOptions) { o.Labels[labelComponent] = componentAgent },
		"another run":        func(o *client.VolumeCreateOptions) { o.Labels[labelRun] = uuid.NewString() },
		"not managed":        func(o *client.VolumeCreateOptions) { delete(o.Labels, labelManaged) },
		"another deployment": func(o *client.VolumeCreateOptions) { o.Labels[labelDeployment] = "other-net" },
		"no deployment":      func(o *client.VolumeCreateOptions) { delete(o.Labels, labelDeployment) },
		"another runner":     func(o *client.VolumeCreateOptions) { o.Labels[runner.LabelRunnerID] = "runner-9" },
		"a drive's label":    func(o *client.VolumeCreateOptions) { o.Labels[labelDrive] = uuid.NewString() },
		"a drive's home":     func(o *client.VolumeCreateOptions) { o.Labels[labelDriveHome] = "alice" },
		"a drive's subject":  func(o *client.VolumeCreateOptions) { o.Labels[labelDriveSubject] = "abc" },
		"another name":       func(o *client.VolumeCreateOptions) { o.Name = "wardyn-dns-other" },
	}
	for _, race := range []bool{false, true} {
		for name, mutate := range cases {
			t.Run(map[bool]string{false: "inspect", true: "create race"}[race]+"/"+name, func(t *testing.T) {
				d, f := dnsTestDriver(Config{})
				held := foreignDNSVolume(d, run, mutate)
				f.volumes[dnsVolumeName(run)] = held
				f.volumeInspectMissesExisting = race
				_, err := d.CreateSandbox(context.Background(), testSpec())
				if !errors.Is(err, errDNSVolumeNotOurs) {
					t.Fatalf("CreateSandbox = %v, want %v", err, errDNSVolumeNotOurs)
				}
				if got := f.volumes[dnsVolumeName(run)]; !maps.Equal(got.Labels, held.Labels) || got.Driver != held.Driver || !maps.Equal(got.DriverOpts, held.DriverOpts) {
					t.Errorf("the foreign volume was changed: %+v, was %+v", got, held)
				}
				if len(f.volumeRemoves) != 0 {
					t.Errorf("the rollback removed %v", f.volumeRemoves)
				}
				if len(f.copies) != 0 || f.containers[dnsSeedName(run)] != nil {
					t.Errorf("something was written toward a foreign volume: copies %d, seed %+v", len(f.copies), f.containers[dnsSeedName(run)])
				}
				if f.containers[agentContainerName(run)] != nil {
					t.Error("an agent was created beside a refused resolver volume")
				}
				if p := f.containers[proxyContainerName(run)]; p != nil && !p.removed {
					t.Error("the proxy survived the refused create")
				}
			})
		}
	}
}

// TestCreateSandbox_AdoptsItsOwnDNSVolume: a volume this driver made for the run (a retry after a crash) is the
// one case an existing volume is used, and it is rewritten with the run's resolver file.
func TestCreateSandbox_AdoptsItsOwnDNSVolume(t *testing.T) {
	d, f := dnsTestDriver(Config{})
	run := testSpec().RunID
	f.volumes[dnsVolumeName(run)] = foreignDNSVolume(d, run, func(*client.VolumeCreateOptions) {})
	if _, err := d.CreateSandbox(context.Background(), testSpec()); err != nil {
		t.Fatalf("CreateSandbox over its own volume: %v", err)
	}
	if len(dnsSeedCopies(f, run)) != 1 || f.volumeCreates != 0 {
		t.Errorf("seed copies %d, VolumeCreate calls %d; want the volume written once and not created again", len(dnsSeedCopies(f, run)), f.volumeCreates)
	}
}

// TestDNSVolumeOwned lists the values the ownership predicate takes. A runner's id is part of it.
func TestDNSVolumeOwned(t *testing.T) {
	d, _ := dnsTestDriver(Config{RunnerID: "runner-1"})
	run := uuid.New()
	good := foreignDNSVolume(d, run, func(*client.VolumeCreateOptions) {})
	vol := func(o client.VolumeCreateOptions) error {
		return d.dnsVolumeOwned(volumeOf(o), run)
	}
	if err := vol(good); err != nil {
		t.Fatalf("its own volume refused: %v", err)
	}
	if good.Labels[runner.LabelRunnerID] != "runner-1" {
		t.Errorf("labels %v carry no runner id", good.Labels)
	}
	other := foreignDNSVolume(d, run, func(o *client.VolumeCreateOptions) { delete(o.Labels, runner.LabelRunnerID) })
	if err := vol(other); !errors.Is(err, errDNSVolumeNotOurs) {
		t.Errorf("a volume with no runner id, on a runner with one: %v, want a refusal", err)
	}
	d2, _ := dnsTestDriver(Config{})
	if err := d2.dnsVolumeOwned(volumeOf(foreignDNSVolume(d, run, func(*client.VolumeCreateOptions) {})), run); !errors.Is(err, errDNSVolumeNotOurs) {
		t.Errorf("a runner's volume on a driver with no runner id: %v, want a refusal", err)
	}
}

// TestTeardown_KeepsAForeignDNSVolume: teardown removes the run's DNS volume only after the same ownership check.
// A foreign object under the name stays, and the teardown still succeeds, including for a run that never mounted
// a resolver (an Engine older than 26 under the opt-out).
func TestTeardown_KeepsAForeignDNSVolume(t *testing.T) {
	for _, engine := range []string{"29.4.2", "25.0.0"} {
		t.Run(engine, func(t *testing.T) {
			d, f := dnsTestDriver(Config{AllowHostReachableRunNetwork: true})
			f.info.ServerVersion = engine
			spec := testSpec()
			sb, err := d.CreateSandbox(context.Background(), spec)
			if err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}
			foreign := foreignDNSVolume(d, spec.RunID, func(o *client.VolumeCreateOptions) { o.Driver = "nfs"; o.Labels = nil })
			f.volumes[dnsVolumeName(spec.RunID)] = foreign
			if err := d.KillSandbox(context.Background(), sb.Ref); err != nil {
				t.Fatalf("KillSandbox: %v", err)
			}
			if got, ok := f.volumes[dnsVolumeName(spec.RunID)]; !ok || got.Driver != "nfs" {
				t.Errorf("the foreign volume was removed or changed: %+v (present %v)", got, ok)
			}
			if len(f.volumeRemoves) != 0 {
				t.Errorf("VolumeRemove calls = %v, want none", f.volumeRemoves)
			}
			if p := f.containers[proxyContainerName(spec.RunID)]; p == nil || !p.removed {
				t.Error("the teardown stopped at the foreign volume and left the proxy")
			}
		})
	}
}

// TestTeardown_RemovesOnlyTheRunsDNSVolume: with a drive mounted too, teardown removes the DNS volume by its exact
// name, not forced, and leaves the drive's storage.
func TestTeardown_RemovesOnlyTheRunsDNSVolume(t *testing.T) {
	d, f := dnsTestDriver(Config{})
	spec := testSpec()
	drive := dockerVolumeDrive()
	spec.Drives = []types.DriveMount{*drive}
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if _, ok := f.volumes[drive.ObjectName]; !ok {
		t.Fatal("the drive's volume was not created")
	}
	if err := d.KillSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatalf("KillSandbox: %v", err)
	}
	if !slices.Equal(f.volumeRemoves, []string{dnsVolumeName(spec.RunID)}) || f.lastVolumeRemoveForce {
		t.Errorf("VolumeRemove calls = %v force=%v, want exactly the run's DNS volume, not forced", f.volumeRemoves, f.lastVolumeRemoveForce)
	}
	if _, ok := f.volumes[drive.ObjectName]; !ok {
		t.Error("teardown removed the drive's volume")
	}
}

// TestTeardown_DNSVolumeGoesBeforeTheProxy: a DNS volume the daemon will not remove stops the teardown with the
// proxy still there, so the orphan sweep has a container to key a retry on.
func TestTeardown_DNSVolumeGoesBeforeTheProxy(t *testing.T) {
	d, f := dnsTestDriver(Config{})
	spec := testSpec()
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	f.volumeInUse = dnsVolumeName(spec.RunID)
	if err := d.KillSandbox(context.Background(), sb.Ref); err == nil {
		t.Fatal("KillSandbox succeeded though the DNS volume could not be removed")
	}
	if p := f.containers[proxyContainerName(spec.RunID)]; p == nil || p.removed {
		t.Fatalf("proxy = %+v after a failed DNS volume removal, want it kept as the retry anchor", p)
	}
	f.volumeInUse = ""
	if err := d.teardown(context.Background(), sb.Ref); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, ok := f.volumes[dnsVolumeName(spec.RunID)]; ok {
		t.Error("the retry left the DNS volume")
	}
	if p := f.containers[proxyContainerName(spec.RunID)]; !p.removed {
		t.Error("the retry left the proxy")
	}
}

// TestTeardown_RemovesALeftDNSSeed: a seed a crash left is removed by the run's teardown, by its run-derived name.
func TestTeardown_RemovesALeftDNSSeed(t *testing.T) {
	f := newFakeDocker()
	id := uuid.New()
	f.containers[dnsSeedName(id)] = &createdContainer{name: dnsSeedName(id)}
	if err := newTestDriver(f).teardown(context.Background(), agentContainerName(id)); err != nil {
		t.Fatal(err)
	}
	if c := f.containers[dnsSeedName(id)]; c != nil && !c.removed {
		t.Errorf("teardown left the DNS seed %s", dnsSeedName(id))
	}
}

// TestCreateSandbox_DNSFailuresRollBackTheVolume: every way the create can fail after the DNS volume exists
// removes the volume and the seed, and leaves neither proxy nor network.
func TestCreateSandbox_DNSFailuresRollBackTheVolume(t *testing.T) {
	run := testSpec().RunID
	cases := map[string]struct {
		arrange func(*fakeDocker, context.CancelFunc)
		wantErr error
	}{
		"the seed cannot be created": {func(f *fakeDocker, _ context.CancelFunc) { f.failCreateContainer = dnsSeedPrefix }, nil},
		"the copy fails":             {func(f *fakeDocker, _ context.CancelFunc) { f.failCopyToContainer = true }, nil},
		"the context ends mid-seed": {func(f *fakeDocker, cancel context.CancelFunc) {
			f.onCreate = func(name string) {
				if strings.HasPrefix(name, dnsSeedPrefix) {
					cancel()
				}
			}
		}, context.Canceled},
		"the agent cannot be created": {func(f *fakeDocker, _ context.CancelFunc) { f.failCreateContainer = "wardyn-agent-" }, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d, f := dnsTestDriver(Config{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tc.arrange(f, cancel)
			_, err := d.CreateSandbox(ctx, testSpec())
			if err == nil || tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateSandbox = %v, want a failure (%v)", err, tc.wantErr)
			}
			if _, ok := f.volumes[dnsVolumeName(run)]; ok {
				t.Error("the DNS volume survived the failed create")
			}
			if c := f.containers[dnsSeedName(run)]; c != nil && !c.removed {
				t.Error("the DNS seed survived the failed create")
			}
			if p := f.containers[proxyContainerName(run)]; p != nil && !p.removed {
				t.Error("the proxy survived the failed create")
			}
			if _, ok := f.networks[internalNetName(run)]; ok {
				t.Error("the network survived the failed create")
			}
		})
	}
}

// TestKeptRun_KeepsItsResolverUntilTeardown: ending a lease keeps the agent and its resolver volume; the revive
// brings the agent back behind a new proxy over the same volume; only the final teardown removes it.
func TestKeptRun_KeepsItsResolverUntilTeardown(t *testing.T) {
	d, f := dnsTestDriver(Config{InternalNetwork: "wardyn-internal"})
	spec := testSpec()
	ctx := context.Background()
	sb, err := d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if err := d.EndSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("EndSandbox: %v", err)
	}
	if _, ok := f.volumes[dnsVolumeName(spec.RunID)]; !ok || len(f.volumeRemoves) != 0 {
		t.Fatalf("after EndSandbox: volume present %v, removals %v; want it kept with the agent", ok, f.volumeRemoves)
	}
	if err := d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "fresh")); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	p := f.containers[proxyContainerName(spec.RunID)]
	if !slices.Contains(p.cfg.Env, proxy.DNSListenEnv+"=:53") {
		t.Errorf("the replacement proxy env = %v, want %s=:53", p.cfg.Env, proxy.DNSListenEnv)
	}
	if err := d.StartSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("StartSandbox: %v", err)
	}
	if m := findMount(f.containers[sb.Ref].host.Mounts, "/etc/resolv.conf"); m == nil || len(f.volumeRemoves) != 0 {
		t.Fatalf("revived agent mount %+v, removals %v; want the same resolver mount and no removal", m, f.volumeRemoves)
	}
	if err := d.KillSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("KillSandbox: %v", err)
	}
	if _, ok := f.volumes[dnsVolumeName(spec.RunID)]; ok {
		t.Error("the final teardown left the DNS volume")
	}
}

// TestSweepOrphanedSandboxes_RemovesLeftoverDNSSeeds: an aged DNS seed of this runner is removed; a young one, another
// runner's and another deployment's are left, and no volume is removed by the sweep.
func TestSweepOrphanedSandboxes_RemovesLeftoverDNSSeeds(t *testing.T) {
	d, f := dnsTestDriver(Config{RunnerID: "runner-1"})
	oldRun, youngRun, otherRunner, otherDeployment := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{oldRun, youngRun, otherRunner, otherDeployment} {
		f.containers[dnsSeedName(id)] = &createdContainer{name: dnsSeedName(id)}
	}
	old := time.Now().Add(-time.Hour).Unix()
	f.listItems = []container.Summary{
		{ID: dnsSeedName(oldRun), Created: old, Labels: d.dnsLabels(oldRun, componentDNSSeed)},
		{ID: dnsSeedName(youngRun), Created: time.Now().Unix(), Labels: d.dnsLabels(youngRun, componentDNSSeed)},
		{ID: dnsSeedName(otherRunner), Created: old, Labels: wardynLabels(otherRunner, componentDNSSeed, map[string]string{runner.LabelRunnerID: "runner-2", labelDeployment: "wardyn-internal"})},
		{ID: dnsSeedName(otherDeployment), Created: old, Labels: wardynLabels(otherDeployment, componentDNSSeed, map[string]string{runner.LabelRunnerID: "runner-1", labelDeployment: "elsewhere"})},
	}
	swept, err := d.SweepOrphanedSandboxes(context.Background(), 10*time.Minute, func(uuid.UUID) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if swept != 1 || !f.containers[dnsSeedName(oldRun)].removed {
		t.Errorf("swept %d, aged seed removed = %v; want just the aged seed", swept, f.containers[dnsSeedName(oldRun)].removed)
	}
	for _, id := range []uuid.UUID{youngRun, otherRunner, otherDeployment} {
		if f.containers[dnsSeedName(id)].removed {
			t.Errorf("the sweep removed %s", dnsSeedName(id))
		}
	}
	if len(f.volumeRemoves) != 0 {
		t.Errorf("the sweep removed volumes %v; the DNS volume goes with its run's teardown only", f.volumeRemoves)
	}
}

// TestSweepOrphanedSandboxes_OrphanedRunLosesItsDNSVolume: an orphaned run keyed by its proxy is torn down whole,
// and its DNS volume goes with it.
func TestSweepOrphanedSandboxes_OrphanedRunLosesItsDNSVolume(t *testing.T) {
	d, f := dnsTestDriver(Config{})
	spec := testSpec()
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).Unix()
	f.listItems = []container.Summary{{ID: proxyContainerName(spec.RunID), Created: old, Labels: d.labels(spec.RunID, componentProxy, nil)}}
	if _, err := d.SweepOrphanedSandboxes(context.Background(), 10*time.Minute, func(uuid.UUID) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.volumes[dnsVolumeName(spec.RunID)]; ok {
		t.Error("the orphan teardown left the run's DNS volume")
	}
}
