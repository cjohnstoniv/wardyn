// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const applicationRunEnv = "WARDYN_APPLICATION_RUN"

// TestApplyDispatchModeEnv_ApplicationRun: only the interactive application
// (#1994) carries the marker that makes an image start its startup command
// again on a revive. It is decided from the resolved run, not from anything
// the request supplied: a startup command plus a declared app, nothing else.
func TestApplyDispatchModeEnv_ApplicationRun(t *testing.T) {
	apps := []types.UIApp{{Name: "web", Port: 3000, Path: "/"}}
	for _, c := range []struct {
		name string
		task string
		p    dispatchParams
		want string
	}{
		{"startup command with a declared app", "npm run dev",
			dispatchParams{Interactive: true, InteractiveStart: "shell", Policy: types.RunPolicySpec{UIApps: apps}}, "1"},
		{"a plain interactive shell run with a seed", "npm run dev",
			dispatchParams{Interactive: true, InteractiveStart: "shell"}, ""},
		{"a starting prompt for an agent beside a declared app", "fix the build",
			dispatchParams{Interactive: true, InteractiveStart: "agent", Policy: types.RunPolicySpec{UIApps: apps}}, ""},
		{"a declared app and no startup command", "",
			dispatchParams{Interactive: true, InteractiveStart: "shell", Policy: types.RunPolicySpec{UIApps: apps}}, ""},
		{"a background run never seeds", "npm run dev",
			dispatchParams{Policy: types.RunPolicySpec{UIApps: apps}}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{}
			applyDispatchModeEnv(env, types.AgentRun{ID: uuid.New(), Task: c.task}, c.p)
			if got := env[applicationRunEnv]; got != c.want {
				t.Errorf("Env[%s] = %q, want %q", applicationRunEnv, got, c.want)
			}
		})
	}
}

// TestApplicationRunEnv_UserEnvCannotSetIt: the marker's name is in the WARDYN_
// namespace, which both writers of user env refuse (component config drops it,
// an env_secret grant is not accepted), and neither overwrites a variable dispatch
// already set. So a user can neither raise it on a run that is not an application
// nor lower it on one that is.
func TestApplicationRunEnv_UserEnvCannotSetIt(t *testing.T) {
	srv, _, _, run := dispatchTeardownFixture(t, &fakeRunner{}, types.RunPending)
	user := []componentConfig{{ordinal: 0, source: componentSourceInline, selfDefined: true,
		env: map[string]string{applicationRunEnv: "1", "KEPT": "yes"}}}

	plain := map[string]string{}
	applyDispatchModeEnv(plain, run, dispatchParams{Interactive: true, InteractiveStart: "shell"})
	srv.applyComponentConfigEnv(context.Background(), run, user, plain)
	if v, ok := plain[applicationRunEnv]; ok || plain["KEPT"] != "yes" {
		t.Errorf("component config on a plain run: Env[%s] = %q (set=%v), KEPT = %q; want the marker absent and the other key kept", applicationRunEnv, v, ok, plain["KEPT"])
	}

	app := map[string]string{}
	run.Task = "npm run dev"
	applyDispatchModeEnv(app, run, dispatchParams{Interactive: true, InteractiveStart: "shell",
		Policy: types.RunPolicySpec{UIApps: []types.UIApp{{Name: "web", Port: 3000, Path: "/"}}}})
	user[0].env[applicationRunEnv] = "0"
	srv.applyComponentConfigEnv(context.Background(), run, user, app)
	if app[applicationRunEnv] != "1" {
		t.Errorf("component config on an application run: Env[%s] = %q, want dispatch's 1", applicationRunEnv, app[applicationRunEnv])
	}

	if err := validateEligibleGrant(0, envSecretGrant(applicationRunEnv, "corp-token")); err == nil {
		t.Errorf("an env_secret grant for %s was accepted, want it refused", applicationRunEnv)
	}
}
