// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package conformance

// ephemeral_disk.go is contract item 7 — ephemeral-disk enforcement — lifted out
// of conformance.go whole when 0.7.5 gave the case its second fill target. It is
// the one case with a timer, a budget the Makefile has to fit, and a scripted
// in-sandbox probe, so it reads better alone than as the tail of the suite file
// (which was also at its size cap).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ephemeralFillTarget is one path the eviction case fills, and one sub-run of
// it. TWO of them, because a substrate's disk budget is claimed over BOTH paths
// its agent is told it may write: /tmp, which carries the per-run CA files, and
// the workdir, which is where a clone at its default destination lands. A gate
// that only ever filled /tmp would go green on a substrate whose workdir writes
// are unbounded — which is exactly the shape 0.7.4 disclosed.
//
// It is not a claim about "everywhere the agent writes", and this case does not
// make one: a run can be authored to write outside both paths, and a toolchain
// cache usually is. What the k8s substrate does and does not reach is stated in
// its own ephemeralScratchVolumes, next to the mounts.
//
// substrateOwned is the difference between the two, and it changes what an
// unwritable target MEANS:
//
//   - /tmp exists in the IMAGE. An image whose /tmp cannot be opened is an
//     environment contract — the fill never ran, so the case says nothing about
//     enforcement either way and skips (exit 91 below).
//   - the workdir is mounted by the SUBSTRATE under test. If it cannot be
//     opened, the substrate did not provide the writable scratch it claims to
//     bound — that is a verdict, and skipping it would hide exactly the defect
//     this target was added to catch.
type ephemeralFillTarget struct {
	name           string
	path           string
	substrateOwned bool
}

// ephemeralFillTargets is the set walked by the eviction sub-case. The workdir
// path is the sandbox contract's own (`/home/agent/work`, WORKDIR in every agent
// image) — spelled literally, because the point is to assert about the path the
// images and the substrate agreed on rather than about a constant they share.
var ephemeralFillTargets = []ephemeralFillTarget{
	{name: "Tmp", path: "/tmp/wardyn-ephemeral-fill"},
	{name: "Workdir", path: "/home/agent/work/wardyn-ephemeral-fill", substrateOwned: true},
}

// ephemeralFillScriptFor writes far past the ephemeral-disk case's 64Mi limit at
// one target. It is a CODED probe, the same rule loopbackRelayListenScript
// follows: a missing applet is an IMAGE verdict, never a substrate one, and it
// says so with its own exit code rather than passing (or failing) silently.
//
// THE EXIT-CODE CONTRACT (pinned by TestEphemeralFillScriptCodesAreDistinct):
//
//	90  no dd applet — an IMAGE contract, not a substrate verdict
//	91  the fill target could not be OPENED — read as an ENVIRONMENT verdict for
//	    an image-owned target and as a SUBSTRATE one for a substrate-owned target
//	    (see ephemeralFillTarget), and in neither case as "does not enforce"
//	0   or any other code: the fill ran. On a substrate that enforces the limit
//	    it never completes, because the pod carrying it is killed.
//
// IT NEVER WRITES UNDER $HOME, and that was finding 2. The agent pod runs as uid
// 1000 with no HOME in its env and no passwd entry for that uid, so runc's
// fallback answers HOME=/ — which is root:root 0755 in busybox. Every fill
// therefore died with `dd: can't open '//wardyn-ephemeral-fill': Permission
// denied` and exit 1: not the coded 90, so the IMAGE skip never fired, nothing
// was written, and the case burned the whole eviction budget before reporting
// "want RunFailed with Evicted" — the false "this substrate does not enforce"
// its own comment says it must never give. The two targets it writes instead are
// each any-uid-writable by construction: /tmp is 1777 in every image this gate
// runs against, and the workdir is a substrate-provided scratch mount.
//
// THE OPEN IS PROBED SEPARATELY from the fill, with dd itself at count=0, so an
// unwritable target is distinguishable from a full one: a zero-byte create
// cannot fail with ENOSPC, which is the outcome the fill below exists to
// provoke. Folding the two would put "the kubelet is evicting us" and "this
// image will not let us write" behind one exit code, and the whole point of the
// case is that those are different verdicts.
func ephemeralFillScriptFor(path string) string {
	return `command -v dd >/dev/null 2>&1 || exit 90
dd if=/dev/zero of=` + path + ` bs=1 count=0 2>/dev/null || exit 91
dd if=/dev/zero of=` + path + ` bs=1M count=256`
}

// ephemeralDiskLimitMiB is the case's limit and ephemeralOversizedDiskMiB the
// second sub-case's. The oversized one is deliberately larger than any test node's
// disk: if the request were ever a COPY of the limit, the pod would not schedule
// and CreateSandbox would fail loudly, rather than the case passing vacuously.
const (
	ephemeralDiskLimitMiB     = 64
	ephemeralOversizedDiskMiB = 200000
)

// ephemeralEvictionBudget bounds the eviction wait. This is the FIRST conformance
// case whose pass depends on a timer, so the budget is deliberately generous: the
// kubelet meters local ephemeral storage on its periodic housekeeping tick (~10s),
// so the verdict lands seconds-to-a-minute after the write — never synchronously
// with it. A tight budget would read a slow node as a substrate that does not
// enforce, which is the one wrong answer this case must not give.
//
// IT IS SPENT ONCE PER FILL TARGET, NOT TWICE PER TARGET (finding 3). Each
// target's sub-run has its own context of opts.timeout()+ephemeralEvictionBudget
// and its post-eviction poll is derived FROM that rather than from a fresh
// Background — r.Wait is bounded to opts.timeout() so it cannot drain the poll's
// share. Summed instead of shared, one sub-run could take 3m+4m+4m = 11m against
// the Makefile's `go test -timeout`, and a -timeout expiry is a panic that kills
// the package and discards every verdict the rest of the suite already produced.
// TestEphemeralCaseBudgetFitsTheMakefileTimeout pins the arithmetic, over the
// whole target list rather than over one target.
const ephemeralEvictionBudget = 4 * time.Minute

// ephemeralFillBudget is ONE fill target's whole sub-run: the per-operation
// timeout for everything up to and including r.Wait, plus one eviction budget for
// the Status poll.
func ephemeralFillBudget(opts Options) time.Duration { return opts.timeout() + ephemeralEvictionBudget }

// ephemeralCaseBudget is what the WHOLE case can cost: one fill budget per
// target, since the targets run in sequence, PLUS the oversized sub-case's own
// operation timeout. That last term is easy to forget and was: the oversized
// sub-case used to share the eviction sub-case's deadline and now takes a fresh
// one, so leaving it out would have the Makefile-timeout pin assert about 14m
// while the case could really spend 17m. Named so that pin can read this rather
// than re-derive the sum it is asserting about.
func ephemeralCaseBudget(opts Options) time.Duration {
	return time.Duration(len(ephemeralFillTargets))*ephemeralFillBudget(opts) + opts.timeout()
}

// testEphemeralDiskLimit is the ephemeral-disk enforcement gate. It runs ONLY on a
// driver that declares EphemeralDiskEnforcement `eviction` — every other word
// means something different (a docker filesystem quota refuses the write and
// surfaces ENOSPC to the agent; `none` binds nothing), so this case skips them by
// name instead of pretending one shape fits all.
//
// Two sub-cases, and the second is the subtle one:
//
//   - Over the limit, the run DIES and says why: the pod is evicted and Status
//     reports RunFailed with "Evicted" in the message (the reason, which carries
//     the whole verdict, joined to the kubelet's detail). Once per fill target —
//     the agent's /tmp and its workdir are both inside the budget or the claim is
//     only half true.
//   - An OVERSIZED limit still schedules, and its accepted request is the small
//     fixed floor. Kubernetes copies a limit into the request when no request is
//     set for that key, so a driver that sent the limit alone would produce pods
//     that need the org's whole ceiling free — Pending, silently, with no
//     container status to narrate. CreateSandbox waiting for the container to run
//     is itself the "it scheduled" assertion; the probe then reads the accepted
//     request back.
func testEphemeralDiskLimit(t *testing.T, r runner.Runner, opts Options) {
	t.Helper()
	// The gate reads are cheap and shared; each fill target then takes its OWN
	// deadline below, because one shared case deadline would let a slow first
	// target eat the second target's whole budget and report it as "does not
	// enforce".
	gateCtx, gateCancel := context.WithTimeout(context.Background(), opts.timeout())
	defer gateCancel()

	caps, err := r.Capabilities(gateCtx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.EphemeralDiskEnforcement != types.StorageEnforcementEviction {
		t.Skipf("driver %q declares ephemeral-disk enforcement %q, not %q — this case asserts the eviction shape specifically",
			r.Name(), caps.EphemeralDiskEnforcement, types.StorageEnforcementEviction)
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Skipf("driver %q declares no ConfinementClasses; no sandbox to fill", r.Name())
	}

	newSandbox := func(t *testing.T, ctx context.Context, diskMiB int64) runner.Sandbox {
		t.Helper()
		spec := minimalSpec(opts.image())
		spec.ConfinementClass = caps.ConfinementClasses[len(caps.ConfinementClasses)-1]
		spec.Resources = runner.Resources{DiskMiB: diskMiB}
		sb, err := r.CreateSandbox(ctx, spec)
		if err != nil {
			t.Fatalf("CreateSandbox with DiskMiB %d: %v", diskMiB, err)
		}
		t.Cleanup(func() { _ = r.StopSandbox(context.Background(), sb.Ref) })
		return sb
	}

	t.Run("OverTheLimitTheRunIsEvicted", func(t *testing.T) {
		for _, target := range ephemeralFillTargets {
			t.Run(target.name, func(t *testing.T) {
				fillAndExpectEviction(t, r, opts, newSandbox, target)
			})
		}
	})

	t.Run("AnOversizedLimitStillSchedules", func(t *testing.T) {
		if opts.EphemeralStorageProbe == nil {
			t.Skip("no EphemeralStorageProbe injected; the accepted-request read-back is the only assertion that can catch a request copied from the limit — wire it in driver CI")
		}
		ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
		defer cancel()
		// A pod that will not schedule is exactly the copy-from-limit bug, and on
		// this substrate CreateSandbox waits for the container to run, so the
		// failure lands here rather than as a silent Pending.
		sb := newSandbox(t, ctx, ephemeralOversizedDiskMiB)
		opts.EphemeralStorageProbe(t, sb.Ref)
	})
}

// fillAndExpectEviction is one fill target's sub-run: fill past the limit at that
// target and wait for the substrate to kill the run for it.
func fillAndExpectEviction(t *testing.T, r runner.Runner, opts Options,
	newSandbox func(*testing.T, context.Context, int64) runner.Sandbox, target ephemeralFillTarget,
) {
	t.Helper()
	// ONE DEADLINE FOR THIS TARGET, and every wait inside it is a slice of this
	// rather than a fresh budget of its own — see ephemeralEvictionBudget.
	ctx, cancel := context.WithTimeout(context.Background(), ephemeralFillBudget(opts))
	defer cancel()

	sb := newSandbox(t, ctx, ephemeralDiskLimitMiB)
	if _, err := r.Exec(ctx, sb.Ref, []string{"sh", "-c", ephemeralFillScriptFor(target.path)}); err != nil {
		// Not fatal: on a substrate that enforces the limit, the exec's own
		// transport dies with the pod it is writing inside, which is the
		// outcome this case is waiting for.
		t.Logf("Exec(fill %s): %v (expected once the pod is killed)", target.path, err)
	}
	// r.Wait IS BOUNDED TO ONE OPERATION'S TIMEOUT, not to this sub-run's whole
	// deadline. On k8s it polls the EXEC ephemeral container, which need never
	// reach Terminated inside a pod the kubelet is killing, so handed the sub-run
	// context it would legitimately drain every minute the poll below still
	// needs. That is what made the poll take a fresh Background budget, and the
	// sum (3m Wait + 4m poll on top of 4m already spent) is what could outrun the
	// package -timeout and discard every verdict.
	waitCtx, waitCancel := context.WithTimeout(ctx, opts.timeout())
	code, werr := r.Wait(waitCtx, sb.Ref)
	waitCancel()
	if werr == nil {
		switch code {
		case 90:
			t.Skipf("sandbox image has no dd applet; cannot fill %s — an IMAGE contract, not a substrate verdict", target.path)
		case 91:
			// FOR AN IMAGE-OWNED TARGET this is an ENVIRONMENT verdict, and
			// deliberately not "the substrate does not enforce": nothing was
			// written, so the kubelet had nothing to meter and the eviction this
			// case asserts was never provoked. Reporting it as a failure of the
			// substrate is the false negative the whole case exists to avoid —
			// this is the shape the uid-1000 $HOME bug wore for as long as it
			// went unnoticed.
			//
			// FOR A SUBSTRATE-OWNED TARGET it is the opposite: the directory is
			// the substrate's to provide, so "could not open it" IS the verdict,
			// and skipping would hide the very gap the target was added for.
			if !target.substrateOwned {
				t.Skipf("sandbox image could not open %s for writing (exit 91); the fill never ran, so this says nothing about %q enforcement — an ENVIRONMENT contract, not a substrate verdict",
					target.path, types.StorageEnforcementEviction)
			}
			t.Fatalf("could not open %s for writing (exit 91) — that directory is the SUBSTRATE's to provide as writable scratch for the agent's uid, so this is a substrate verdict: a budget over a path the agent cannot write bounds nothing",
				target.path)
		}
	}

	// The poll's budget is a SLICE OF THIS SUB-RUN's, derived from ctx rather
	// than from a fresh Background: each fill target gets ONE deadline and every
	// wait inside it spends part of that, never a new allowance on top.
	// Cancelled on return, so nothing outlives the sub-run.
	pollCtx, pollCancel := context.WithTimeout(ctx, ephemeralEvictionBudget)
	defer pollCancel()
	deadline, _ := pollCtx.Deadline()
	var last runner.Status
	for time.Now().Before(deadline) {
		st, err := r.Status(pollCtx, sb.Ref)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		last = st
		if st.State == types.RunFailed && strings.Contains(st.Message, "Evicted") {
			// The kubelet's own detail names WHICH budget bound first (the
			// volume's sizeLimit or the pod's ephemeral-storage total), which is
			// the one thing a green run here should still put in the log.
			t.Logf("evicted after filling %s: %s", target.path, st.Message)
			return
		}
		time.Sleep(2 * time.Second)
	}
	// "up to", because the poll shares this sub-run's one deadline: it stops at
	// whichever comes first, its own budget or what the sub-run has left.
	t.Errorf("after up to %s the run is state=%q message=%q; want RunFailed with \"Evicted\" — a %dMiB limit and a 256MiB write at %s means the kubelet should have killed the pod",
		ephemeralEvictionBudget, last.State, last.Message, ephemeralDiskLimitMiB, target.path)
}
