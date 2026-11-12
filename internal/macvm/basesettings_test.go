// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/basesettings"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// baseGuest is a guest with the base-settings route: it records the order of the stage's calls and answers
// the route with res and err.
type baseGuest struct {
	*itemGuest
	omu    sync.Mutex
	order  []string
	scopes []string
	res    guestagent.BaseSettingsResult
	err    error
}

func (g *baseGuest) note(s string) {
	g.omu.Lock()
	g.order = append(g.order, s)
	g.omu.Unlock()
}

func (g *baseGuest) BaseSettings(_ context.Context, scope string) (guestagent.BaseSettingsResult, error) {
	g.note("base-settings")
	g.omu.Lock()
	g.scopes = append(g.scopes, scope)
	g.omu.Unlock()
	return g.res, g.err
}

func (g *baseGuest) Posture(context.Context) (guestagent.ToolsPosture, error) {
	g.note("posture")
	return guestagent.ToolsPosture{}, nil
}

func (g *baseGuest) AgentExec(ctx context.Context, argv, env []string) (string, int, error) {
	g.note("selftest")
	return g.itemGuest.AgentExec(ctx, argv, env)
}

func (g *baseGuest) stage(f *toolsFixture) *macvm.ToolsStage {
	return &macvm.ToolsStage{ToolsOptions: f.opts,
		Boot: func(context.Context, macvm.MachineSpec) (macvm.ToolsGuest, func(context.Context) error, error) {
			return g, func(context.Context) error { return nil }, nil
		}}
}

func newBaseGuest() *baseGuest { return &baseGuest{itemGuest: newItemGuest()} }

const predates = "agent predates this setting; run `wardyn-runnerd macos-vm update-tools` again"

// A tools update applies the base settings after it has read the posture and before the launcher's self-test.
func TestToolsUpdateAppliesBaseSettingsBetweenPostureAndSelftest(t *testing.T) {
	f, g := newTools(t), newBaseGuest()
	res := runStage(t, g.stage(f))
	if len(res.BaseSettingsSkipped) != 0 {
		t.Fatalf("skipped %+v", res.BaseSettingsSkipped)
	}
	i, j, k := slices.Index(g.order, "posture"), slices.Index(g.order, "base-settings"), slices.Index(g.order, "selftest")
	if i < 0 || !(i < j && j < k) {
		t.Fatalf("order %v: want posture, base-settings, selftest", g.order)
	}
	if !slices.Equal(g.scopes, []string{""}) {
		t.Fatalf("the update asked for scopes %q, want the whole table (no scope)", g.scopes)
	}
}

// What the route could not do is recorded, not fatal.
func TestToolsUpdateRecordsSkippedBaseSettings(t *testing.T) {
	f, g := newTools(t), newBaseGuest()
	g.res = guestagent.BaseSettingsResult{Applied: 30, Skipped: []basesettings.Skipped{{Name: "SetupAssistant.DidSeeCloudSetup", Reason: "nobody at the console"}}}
	res := runStage(t, g.stage(f))
	if want := []basesettings.Skipped{{Name: "SetupAssistant.DidSeeCloudSetup", Reason: "nobody at the console"}}; !slices.Equal(res.BaseSettingsSkipped, want) {
		t.Fatalf("skipped %+v, want %+v", res.BaseSettingsSkipped, want)
	}
}

// The base's old agent answers the first update after an upgrade, and has no route: its 404 is recorded as
// the words that say to run the update again, and the update does not fail.
func TestToolsUpdateRecordsAnOldAgentsMissingRoute(t *testing.T) {
	f, g := newTools(t), newBaseGuest()
	g.err = fmt.Errorf("%w: %q", guestagent.ErrNoExec, "404 page not found")
	res := runStage(t, g.stage(f))
	want := []basesettings.Skipped{{Name: "base-settings", Reason: predates}}
	if !slices.Equal(res.BaseSettingsSkipped, want) {
		t.Fatalf("skipped %+v, want %+v", res.BaseSettingsSkipped, want)
	}
}

// Any other failure of the route is a skipped entry as well, and the self-test still runs.
func TestToolsUpdateBaseSettingsFailureIsNotFatal(t *testing.T) {
	f, g := newTools(t), newBaseGuest()
	g.err = fmt.Errorf("guestagent: 500 Internal Server Error: %q", "the guest's macOS version was not read")
	res := runStage(t, g.stage(f))
	if len(res.BaseSettingsSkipped) != 1 || res.BaseSettingsSkipped[0].Name != "base-settings" || !strings.Contains(res.BaseSettingsSkipped[0].Reason, "500") {
		t.Fatalf("skipped %+v", res.BaseSettingsSkipped)
	}
	if !slices.Contains(g.order, "selftest") {
		t.Fatalf("order %v: no self-test after a failed route", g.order)
	}
}

// A guest without the route at all (a fake, or an older host session) records nothing.
func TestToolsUpdateWithoutTheRouteRecordsNothing(t *testing.T) {
	f, g := newTools(t), newGuest()
	var boots, stops atomic.Int64
	res := runStage(t, f.stage(g, &boots, &stops))
	if len(res.BaseSettingsSkipped) != 0 {
		t.Fatalf("skipped %+v", res.BaseSettingsSkipped)
	}
}

// tools.json carries the skipped rows as base_settings_skipped: [{name, reason}], and omits the field when
// none were skipped.
func TestBaseSettingsSkippedIsInToolsJSON(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	g := newBaseGuest()
	g.err = fmt.Errorf("%w: %q", guestagent.ErrNoExec, "404 page not found")
	f.opts.Stage2 = g.stage(tf)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	rec, err := macvm.ReadToolsRecord(f.opts.Layout.BaseDir(0))
	if err != nil || !slices.Equal(rec.BaseSettingsSkipped, []basesettings.Skipped{{Name: "base-settings", Reason: predates}}) {
		t.Fatalf("record %+v %v", rec.BaseSettingsSkipped, err)
	}
	b, _ := json.Marshal(rec)
	if !strings.Contains(string(b), `"base_settings_skipped":[{"name":"base-settings","reason":"`+predates+`"}]`) {
		t.Fatalf("tools.json: %s", b)
	}
	b, _ = json.Marshal(macvm.ToolsRecord{})
	if strings.Contains(string(b), "base_settings_skipped") {
		t.Fatalf("an empty record names the field: %s", b)
	}
}

// edit-base's recheck kills Terminal and removes its saved state with the route's terminal scope, and a
// guest that cannot (an old agent) does not discard the edit.
func TestEditBaseRecheckCleansTerminal(t *testing.T) {
	for name, err := range map[string]error{"applied": nil, "old agent": fmt.Errorf("%w: %q", guestagent.ErrNoExec, "404 page not found")} {
		t.Run(name, func(t *testing.T) {
			e := newEdit(t)
			bg := &baseEditGuest{editGuest: e.g, err: err}
			e.opts.Guest = func(context.Context, macvm.MachineSpec) (macvm.ToolsGuest, func(context.Context) error, error) {
				e.boots++
				return bg, func(context.Context) error { return nil }, nil
			}
			if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
				t.Fatalf("the edit failed: %v", err)
			}
			if !slices.Equal(bg.scopes, []string{"terminal"}) {
				t.Fatalf("scopes %q, want one terminal-scope call", bg.scopes)
			}
		})
	}
}

// baseEditGuest is the edit's headless guest with the route.
type baseEditGuest struct {
	*editGuest
	scopes []string
	err    error
}

func (g *baseEditGuest) BaseSettings(_ context.Context, scope string) (guestagent.BaseSettingsResult, error) {
	g.scopes = append(g.scopes, scope)
	return guestagent.BaseSettingsResult{}, g.err
}
