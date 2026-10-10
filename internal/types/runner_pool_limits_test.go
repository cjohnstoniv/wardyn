// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func amount(def, cap int) RunnerPoolAmount { return RunnerPoolAmount{Default: def, Cap: cap} }

func finite(def, max int) RunnerPoolDuration { return RunnerPoolDuration{DefaultSec: def, MaxSec: max} }

func limitsFixture() RunnerPoolLimits {
	return RunnerPoolLimits{
		Background: &RunnerPoolBackgroundLimits{CPUMillis: amount(2000, 8000), MemoryMiB: amount(4096, 16384), Lifetime: finite(3600, 86400)},
		Interactive: &RunnerPoolInteractiveLimits{
			CPUMillis: amount(1000, 4000), MemoryMiB: amount(2048, 8192), Lifetime: finite(28800, 86400), Idle: finite(1800, 7200),
		},
		Barriers: []ConfinementClass{CC1, CC2},
	}
}

func TestRunnerPoolLimitsValidate(t *testing.T) {
	one := 1
	zero := 0
	for _, tc := range []struct {
		name   string
		mutate func(*RunnerPoolLimits)
		want   string // "" is valid
	}{
		{"both run types", func(*RunnerPoolLimits) {}, ""},
		{"an app server pool: unlimited lifetime, never idle, default no end", func(l *RunnerPoolLimits) {
			l.Background.Lifetime = RunnerPoolDuration{Unlimited: true}
			l.Interactive.Lifetime = RunnerPoolDuration{DefaultSec: 3600, Unlimited: true}
			l.Interactive.Idle = RunnerPoolDuration{Unlimited: true}
		}, ""},
		{"background only", func(l *RunnerPoolLimits) { l.Interactive = nil }, ""},
		{"interactive only", func(l *RunnerPoolLimits) { l.Background = nil }, ""},
		{"a concurrency cap", func(l *RunnerPoolLimits) { l.MaxConcurrentRuns = &one }, ""},
		{"no run type", func(l *RunnerPoolLimits) { l.Background, l.Interactive = nil, nil }, "at least one run type"},
		{"no barrier", func(l *RunnerPoolLimits) { l.Barriers = nil }, "at least one barrier"},
		{"empty barrier list", func(l *RunnerPoolLimits) { l.Barriers = []ConfinementClass{} }, "at least one barrier"},
		{"unknown barrier", func(l *RunnerPoolLimits) { l.Barriers = []ConfinementClass{"CC9"} }, "not CC1, CC2 or CC3"},
		{"repeated barrier", func(l *RunnerPoolLimits) { l.Barriers = []ConfinementClass{CC2, CC2} }, "listed twice"},
		{"cpu default past cap", func(l *RunnerPoolLimits) { l.Background.CPUMillis = amount(9000, 8000) }, "background.cpu_millis: the default (9000) is past the cap (8000)"},
		{"memory default past cap", func(l *RunnerPoolLimits) { l.Interactive.MemoryMiB = amount(9000, 8192) }, "interactive.memory_mib: the default"},
		{"cpu zero default", func(l *RunnerPoolLimits) { l.Background.CPUMillis = amount(0, 8000) }, "at least 1 milli-CPU"},
		{"memory zero cap", func(l *RunnerPoolLimits) { l.Background.MemoryMiB = amount(0, 0) }, "at least 1 MiB"},
		{"cpu over the request contract", func(l *RunnerPoolLimits) { l.Background.CPUMillis = amount(1, RunnerPoolCPUMillisMax+1) }, "at most"},
		{"memory over the request contract", func(l *RunnerPoolLimits) { l.Background.MemoryMiB = amount(1, RunnerPoolMemoryMiBMax+1) }, "at most"},
		{"lifetime default past max", func(l *RunnerPoolLimits) { l.Background.Lifetime = finite(90000, 86400) }, "background.lifetime: the default (90000) is past the maximum (86400)"},
		{"lifetime with neither max nor unlimited", func(l *RunnerPoolLimits) { l.Background.Lifetime = RunnerPoolDuration{DefaultSec: 5} }, "max_sec to a number of seconds, or unlimited"},
		{"lifetime with both max and unlimited", func(l *RunnerPoolLimits) {
			l.Background.Lifetime = RunnerPoolDuration{MaxSec: 60, DefaultSec: 5, Unlimited: true}
		}, "not both"},
		{"lifetime finite with default 0", func(l *RunnerPoolLimits) { l.Background.Lifetime = finite(0, 60) }, "default of at least 1 second"},
		{"lifetime negative default", func(l *RunnerPoolLimits) { l.Background.Lifetime = RunnerPoolDuration{DefaultSec: -1, Unlimited: true} }, "0 or more seconds"},
		{"lifetime negative max", func(l *RunnerPoolLimits) { l.Background.Lifetime = RunnerPoolDuration{DefaultSec: 1, MaxSec: -5} }, "max_sec to a number"},
		{"lifetime past the seconds bound", func(l *RunnerPoolLimits) { l.Background.Lifetime = finite(1, math.MaxInt32+1) }, "seconds are at most"},
		{"idle default past max", func(l *RunnerPoolLimits) { l.Interactive.Idle = finite(9000, 7200) }, "interactive.idle: the default (9000) is past the maximum (7200)"},
		{"idle with neither max nor never", func(l *RunnerPoolLimits) { l.Interactive.Idle = RunnerPoolDuration{} }, "interactive.idle: set max_sec"},
		{"concurrency cap zero", func(l *RunnerPoolLimits) { l.MaxConcurrentRuns = &zero }, "max_concurrent_runs"},
	} {
		l := limitsFixture()
		tc.mutate(&l)
		err := l.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err = %v, want it to say %q", tc.name, err, tc.want)
		}
	}
}

// TestBackgroundLimitsHaveNoIdle: the idle stop is Interactive-only, so a Background
// body that sends one never decodes, and the encoded form carries none.
func TestBackgroundLimitsHaveNoIdle(t *testing.T) {
	body := `{"background":{"cpu_millis":{"default":1,"cap":2},"memory_mib":{"default":1,"cap":2},` +
		`"lifetime":{"default_sec":1,"max_sec":2},"idle":{"default_sec":1,"max_sec":2}},"barriers":["CC1"]}`
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	var l RunnerPoolLimits
	if err := dec.Decode(&l); err == nil || !strings.Contains(err.Error(), "idle") {
		t.Fatalf("a background body with idle decoded: %v", err)
	}
	raw, err := json.Marshal(limitsFixture().Background)
	if err != nil || bytes.Contains(raw, []byte("idle")) {
		t.Fatalf("background wire = %s, %v", raw, err)
	}
	raw, _ = json.Marshal(limitsFixture().Interactive)
	if !bytes.Contains(raw, []byte(`"idle":{"default_sec":1800,"max_sec":7200}`)) {
		t.Fatalf("interactive wire = %s", raw)
	}
}

// TestUnlimitedIsNeverAnOmittedNumber: the encodings are distinct, and a finite
// limit that lost its max does not validate as unlimited.
func TestUnlimitedIsNeverAnOmittedNumber(t *testing.T) {
	raw, _ := json.Marshal(RunnerPoolDuration{Unlimited: true})
	if string(raw) != `{"default_sec":0,"unlimited":true}` {
		t.Fatalf("unlimited wire = %s", raw)
	}
	var d RunnerPoolDuration
	if err := json.Unmarshal([]byte(`{"default_sec":60}`), &d); err != nil {
		t.Fatal(err)
	}
	l := limitsFixture()
	l.Background.Lifetime = d
	if l.Validate() == nil {
		t.Fatal("a lifetime with only a default validated as unlimited")
	}
}

func TestRunnerPoolLimitsAllowance(t *testing.T) {
	l := limitsFixture()
	if got := l.RunTypes(); len(got) != 2 || got[0] != RunnerPoolRunBackground || got[1] != RunnerPoolRunInteractive {
		t.Fatalf("run types = %v", got)
	}
	l.Interactive = nil
	if l.AllowsRunType(RunnerPoolRunInteractive) || !l.AllowsRunType(RunnerPoolRunBackground) || l.AllowsRunType("batch") {
		t.Fatal("allowance follows which run type has limits")
	}
	if !l.AllowsBarrier(CC1) || !l.AllowsBarrier(CC2) || l.AllowsBarrier(CC3) || l.AllowsBarrier("") {
		t.Fatal("barrier allowance is the listed set")
	}
	if BarrierLabel(CC1) != "Fence" || BarrierLabel(CC2) != "Wall" || BarrierLabel(CC3) != "Vault" || BarrierLabel("") != "" {
		t.Fatal("barrier labels")
	}
}

func TestEffectiveLimitApply(t *testing.T) {
	capped := EffectiveLimit{Default: LimitAmount{Value: 2000}, Cap: LimitAmount{Value: 4000}}
	open := EffectiveLimit{Default: LimitAmount{Unlimited: true}, Cap: LimitAmount{Unlimited: true}}
	for _, tc := range []struct {
		name   string
		l      EffectiveLimit
		asked  int
		want   LimitAmount
		capped bool
	}{
		{"zero asks for the default", capped, 0, LimitAmount{Value: 2000}, false},
		{"within the cap", capped, 3000, LimitAmount{Value: 3000}, false},
		{"at the cap", capped, 4000, LimitAmount{Value: 4000}, false},
		{"past the cap is reduced", capped, 8000, LimitAmount{Value: 4000}, true},
		{"nothing past an unlimited cap", open, 1 << 30, LimitAmount{Value: 1 << 30}, false},
		{"unlimited default", open, 0, LimitAmount{Unlimited: true}, false},
	} {
		got, c := tc.l.Apply(tc.asked)
		if got != tc.want || c != tc.capped {
			t.Errorf("%s: Apply(%d) = %+v, %v; want %+v, %v", tc.name, tc.asked, got, c, tc.want, tc.capped)
		}
	}
}

func TestRunEndReason(t *testing.T) {
	if !RunEndMaxLifetimeReached.Valid() || RunEndReason("max_age").Valid() || RunEndReason("").Valid() {
		t.Fatal("the end reason set is closed")
	}
	if RunEndMaxLifetimeReached != "max_lifetime_reached" || !strings.Contains(RunEndMaxLifetimeReached.Sentence(), "maximum lifetime") {
		t.Fatalf("reason %q sentence %q", RunEndMaxLifetimeReached, RunEndMaxLifetimeReached.Sentence())
	}
	if RunEndReason("x").Sentence() != "" {
		t.Fatal("an unknown reason has no sentence")
	}
}

func TestRunnerPoolWithoutLimitsAddsNoBound(t *testing.T) {
	raw, _ := json.Marshal(RunnerPool{Name: "legacy"})
	if strings.Contains(string(raw), "limits") {
		t.Fatalf("a pool with no limits sent %s", raw)
	}
}
