// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const execFormatLog = "exec /x: exec format error"

// archDriver is an engine reporting `engine` (uname style) holding images of the given
// architectures; the arch probe, if started, exits 255 with an exec-format log.
func archDriver(engine string, images map[string]string) (*Driver, *fakeDocker) {
	f := newFakeDocker()
	f.info.Architecture = engine
	f.archProbeExit = 255
	f.archProbeLog = execFormatLog
	for ref, a := range images {
		f.images[ref] = true
		f.imageArch[ref] = a
	}
	return newTestDriver(f), f
}

// probes names the arch-probe containers the fake saw; others counts every other container.
func probes(f *fakeDocker) (probe []*createdContainer, others int) {
	for name, c := range f.containers {
		if strings.HasPrefix(name, archProbeNamePrefix) {
			probe = append(probe, c)
		} else {
			others++
		}
	}
	return probe, others
}

func TestEnsureImageArchSkipsTheProbeWhenNothingMismatches(t *testing.T) {
	const ref = "mirror.example/agent:1"
	cases := []struct{ name, engine, image string }{
		{"match", "x86_64", "amd64"},
		{"aarch64 is arm64", "aarch64", "arm64"},
		{"image arch unknown", "aarch64", ""},
		{"engine arch unknown", "", "amd64"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, f := archDriver(c.engine, map[string]string{ref: c.image})
			if err := d.ensureImage(context.Background(), ref, "", nil); err != nil {
				t.Fatalf("got %v, want nil", err)
			}
			if p, _ := probes(f); len(p) != 0 {
				t.Errorf("%d probe containers started, want none", len(p))
			}
		})
	}
}

func TestEnsureImageArchRefusesOnProvenExecFormat(t *testing.T) {
	const ref = "mirror.example/agent:1"
	for _, c := range []struct{ engine, image, eng string }{
		{"aarch64", "amd64", "arm64"},
		{"x86_64", "arm64", "amd64"},
	} {
		d, f := archDriver(c.engine, map[string]string{ref: c.image})
		err := d.ensureImage(context.Background(), ref, "", nil)
		if !errors.Is(err, ErrImageArch) || !errors.Is(err, runner.ErrImageArch) {
			t.Fatalf("got %v, want ErrImageArch", err)
		}
		want := "this runner's engine is " + c.eng + " and cannot run " + ref + ", a single-architecture " + c.image +
			" image (exec format error). Name an image for " + c.eng + " or publish a multi-arch one"
		if err.Error() != want {
			t.Errorf("message %q, want %q", err, want)
		}
		p, others := probes(f)
		if len(p) != 1 || !p[0].removed || others != 0 {
			t.Errorf("probes %d (removed %v), other containers %d: want one removed probe and nothing else", len(p), len(p) == 1 && p[0].removed, others)
		}
	}
}

func TestArchProbeIsConfined(t *testing.T) {
	d, f := archDriver("aarch64", map[string]string{"a:1": "amd64"})
	_ = d.ensureImage(context.Background(), "a:1", "", nil)
	p, _ := probes(f)
	if len(p) != 1 {
		t.Fatalf("%d probes", len(p))
	}
	c := p[0]
	if string(c.host.NetworkMode) != "none" || len(c.host.Binds) != 0 || len(c.host.Mounts) != 0 ||
		!slices.Equal(c.host.CapDrop, []string{"ALL"}) || !c.host.ReadonlyRootfs ||
		c.cfg.User != "1000" || c.cfg.Image != "a:1" || len(c.cfg.Entrypoint) != 0 || len(c.cfg.Cmd) != 0 ||
		!slices.Equal(c.host.SecurityOpt, []string{"no-new-privileges"}) || c.host.PidsLimit == nil || len(c.cfg.Env) != 0 ||
		c.cfg.Labels[labelComponent] != componentArchProbe {
		t.Errorf("probe not confined: cfg %+v host %+v", c.cfg, c.host)
	}
}

func TestEnsureImageArchAllowsAnEmulatedImage(t *testing.T) {
	const ref = "mirror.example/agent:1"
	setup := map[string]func(f *fakeDocker){
		"exits 1 with other text": func(f *fakeDocker) { f.archProbeExit = 1; f.archProbeLog = "usage: agent" },
		"exits 0":                 func(f *fakeDocker) { f.archProbeExit = 0; f.archProbeLog = "" },
		"still running":           func(f *fakeDocker) { f.archProbeRunning = true },
	}
	old := archProbeWait
	archProbeWait = 20 * time.Millisecond
	t.Cleanup(func() { archProbeWait = old })
	for name, set := range setup {
		t.Run(name, func(t *testing.T) {
			d, f := archDriver("aarch64", map[string]string{ref: "amd64"})
			set(f)
			if err := d.ensureImage(context.Background(), ref, "", nil); err != nil {
				t.Fatalf("got %v, want nil", err)
			}
			if p, _ := probes(f); len(p) != 1 || !p[0].removed {
				t.Errorf("want one removed probe, got %d", len(p))
			}
		})
	}
}

func TestEnsureImageArchChecksAPulledImage(t *testing.T) {
	d, f := archDriver("aarch64", nil)
	f.onPull = func(ref string) { f.imageArch[ref] = "amd64" }
	if err := d.ensureImage(context.Background(), "mirror.example/agent:2", "", nil); !errors.Is(err, ErrImageArch) {
		t.Fatalf("got %v, want ErrImageArch", err)
	}
}

func TestArchVerdictIsCachedPerForeignArch(t *testing.T) {
	d, f := archDriver("aarch64", map[string]string{"a:1": "amd64", "b:1": "amd64"})
	f.archProbeExit, f.archProbeLog = 0, ""
	if err := d.ensureImage(context.Background(), "a:1", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := d.ensureImage(context.Background(), "b:1", "", nil); err != nil {
		t.Fatal(err)
	}
	if p, _ := probes(f); len(p) != 1 {
		t.Errorf("%d probes for two runnable images of one architecture, want 1", len(p))
	}
}

// One image's own output must not decide for the next image of its architecture: a refusal is
// never cached, so the next image is probed afresh and allowed.
func TestArchRefusalIsNotCachedForOtherImages(t *testing.T) {
	for name, set := range map[string]func(f *fakeDocker){
		"log says exec format error": func(f *fakeDocker) { f.archProbeExit = 0 },
		"start says exec format error": func(f *fakeDocker) {
			f.failStartContainer, f.failStartMsg = archProbeNamePrefix, "exec: \"/exec format error\": stat: no such file"
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, f := archDriver("aarch64", map[string]string{"a:1": "amd64", "b:1": "amd64"})
			set(f)
			if err := d.ensureImage(context.Background(), "a:1", "", nil); !errors.Is(err, ErrImageArch) {
				t.Fatalf("got %v, want ErrImageArch", err)
			}
			f.failStartContainer, f.archProbeExit, f.archProbeLog = "", 0, ""
			if err := d.ensureImage(context.Background(), "b:1", "", nil); err != nil {
				t.Fatalf("the next image of the architecture must be probed afresh and allowed, got %v", err)
			}
			if p, _ := probes(f); len(p) != 2 {
				t.Errorf("%d probes, want 2", len(p))
			}
		})
	}
}

// A runnable verdict under one runtime says nothing about another.
func TestArchVerdictIsCachedPerRuntime(t *testing.T) {
	d, f := archDriver("aarch64", map[string]string{"a:1": "amd64"})
	f.archProbeExit, f.archProbeLog = 0, ""
	if err := d.ensureImage(context.Background(), "a:1", "", nil); err != nil {
		t.Fatal(err)
	}
	f.archProbeExit, f.archProbeLog = 255, execFormatLog
	if err := d.ensureImage(context.Background(), "a:1", "runsc", nil); !errors.Is(err, ErrImageArch) {
		t.Fatalf("got %v, want the runsc probe to refuse", err)
	}
}

// The image-architecture probe of a run's image starts it only under the run's class runtime.
func TestCreateSandboxProbesTheImageUnderTheClassRuntime(t *testing.T) {
	for class, rt := range map[types.ConfinementClass]string{types.CC1: "", types.CC2: "runsc", types.CC3: "kata-runtime"} {
		t.Run(string(class), func(t *testing.T) {
			d, f := archDriver("aarch64", map[string]string{testSpec().Image: "amd64", "wardyn-proxy:dev": "arm64"})
			arch := f.info.Architecture
			f.info = infoWithRuntimes("runsc", "kata-runtime")
			f.info.Architecture = arch
			f.archProbeExit, f.archProbeLog = 0, ""
			spec := testSpec()
			spec.ConfinementClass = class
			if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}
			p, _ := probes(f)
			agent := f.containers[agentContainerName(spec.RunID)]
			if len(p) != 1 || agent == nil {
				t.Fatalf("%d probes, agent %v", len(p), agent != nil)
			}
			if p[0].host.Runtime != rt || agent.host.Runtime != rt {
				t.Errorf("probe runtime %q, agent runtime %q, want %q for both", p[0].host.Runtime, agent.host.Runtime, rt)
			}
		})
	}
}

func TestArchProbeStartErrorWithExecFormatRefuses(t *testing.T) {
	d, f := archDriver("aarch64", map[string]string{"a:1": "amd64"})
	f.failStartContainer = archProbeNamePrefix
	f.failStartMsg = "exec container process `/x`: Exec format error"
	if err := d.ensureImage(context.Background(), "a:1", "", nil); !errors.Is(err, ErrImageArch) {
		t.Fatalf("got %v, want ErrImageArch", err)
	}
	if p, _ := probes(f); len(p) != 1 || !p[0].removed {
		t.Errorf("want one removed probe")
	}
}

// An inconclusive probe allows the image this once and caches nothing: after the fault clears, the
// next image of the same architecture is probed again and refused.
func TestInconclusiveArchProbeIsNotCached(t *testing.T) {
	cases := map[string]struct {
		set   func(f *fakeDocker, cancel func())
		clear func(f *fakeDocker)
	}{
		"create fails (no command)": {
			func(f *fakeDocker, _ func()) { f.failCreateContainer = archProbeNamePrefix },
			func(f *fakeDocker) { f.failCreateContainer = "" }},
		"start fails for another reason": {
			func(f *fakeDocker, _ func()) { f.failStartContainer = archProbeNamePrefix },
			func(f *fakeDocker) { f.failStartContainer = "" }},
		"wait errors": {
			func(f *fakeDocker, _ func()) { f.waitErrs = 1 },
			func(f *fakeDocker) { f.waitErrs = 0 }},
		"log unreadable": {
			func(f *fakeDocker, _ func()) { f.failLogs = true },
			func(f *fakeDocker) { f.failLogs = false }},
		"ctx cancelled": {
			func(f *fakeDocker, cancel func()) { f.archProbeRunning = true; f.onCreate = func(string) { cancel() } },
			func(f *fakeDocker) { f.archProbeRunning = false; f.onCreate = nil }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d, f := archDriver("aarch64", map[string]string{"a:1": "amd64", "b:1": "amd64"})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c.set(f, cancel)
			if err := d.ensureImage(ctx, "a:1", "", nil); err != nil {
				t.Fatalf("an inconclusive probe must allow the image, got %v", err)
			}
			c.clear(f)
			if err := d.ensureImage(context.Background(), "b:1", "", nil); !errors.Is(err, ErrImageArch) {
				t.Fatalf("the next image of the architecture must be probed afresh and refused, got %v", err)
			}
		})
	}
}

func TestCreateSandboxRefusesAnUnrunnableImage(t *testing.T) {
	for name, images := range map[string]map[string]string{
		"agent image": {testSpec().Image: "amd64", "wardyn-proxy:dev": "arm64"},
		"proxy image": {testSpec().Image: "arm64", "wardyn-proxy:dev": "amd64"},
	} {
		t.Run(name, func(t *testing.T) {
			d, f := archDriver("aarch64", images)
			_, err := d.CreateSandbox(context.Background(), testSpec())
			if !errors.Is(err, ErrImageArch) {
				t.Fatalf("got %v, want ErrImageArch", err)
			}
			if _, others := probes(f); others != 0 || len(f.networks) != 0 {
				t.Errorf("created %d other containers, %d networks before refusing", others, len(f.networks))
			}
		})
	}
}

func TestProbeRelayRefusesAnUnrunnableProxyImage(t *testing.T) {
	d, f := archDriver("aarch64", map[string]string{"wardyn-proxy:dev": "amd64"})
	err := d.ProbeRelay(context.Background(), relayProbeTarget, 8443)
	if !errors.Is(err, ErrImageArch) {
		t.Fatalf("got %v, want ErrImageArch", err)
	}
	if _, others := probes(f); others != 0 {
		t.Errorf("a relay probe container was created")
	}
}

func TestProbeDriveRefusesAnUnrunnableImage(t *testing.T) {
	d, f := archDriver("aarch64", map[string]string{defaultDriveProbeImage: "amd64"})
	_, err := d.ProbeDrive(context.Background(), probeDriveFixture(t, d))
	if !errors.Is(err, ErrImageArch) {
		t.Fatalf("got %v, want ErrImageArch", err)
	}
	if _, others := probes(f); others != 0 {
		t.Errorf("a drive probe container was created")
	}
}

func TestEngineArchIsCached(t *testing.T) {
	d, f := archDriver("aarch64", nil)
	if got := d.engineArch(context.Background()); got != "arm64" {
		t.Fatalf("engineArch = %q", got)
	}
	f.info.Architecture = "x86_64"
	if got := d.engineArch(context.Background()); got != "arm64" {
		t.Fatalf("engineArch after the daemon changed = %q, want the cached arm64", got)
	}
}

func TestCheckImageArchSkipsAnImageGoneSinceThePresenceCheck(t *testing.T) {
	d, _ := archDriver("aarch64", nil)
	if err := d.checkImageArch(context.Background(), "absent:1", ""); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}
