// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// retiredModelEnvPrefixes and retiredModelEnvNames are the boot variables of
// the operator-held model credential lanes 0.8.2 retires (multi-provider design
// §2.11): the gateway, Bedrock and model knobs, and the shared-subscription
// switches. Model access is a model provider's now, so a deployment still
// setting one would silently lose what it configured; boot refuses instead.
var (
	retiredModelEnvPrefixes = []string{"WARDYN_ANTHROPIC_", "WARDYN_OPENAI_", "WARDYN_BEDROCK_"}
	retiredModelEnvNames    = []string{"WARDYN_AGENT_ANTHROPIC_MODEL", "WARDYN_SUBSCRIPTION_INJECT", "WARDYN_ALLOW_SHARED_SUBSCRIPTION"}
)

// retiredModelEnvInert reports whether a retired variable's value configures
// nothing: empty, or the "false"/"off" the 0.7 compose files render by default.
func retiredModelEnvInert(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "false", "off":
		return true
	}
	return false
}

// refuseRetiredModelEnv refuses boot when environ (os.Environ's shape) sets a
// retired model variable to anything but an inert value, naming every one.
func refuseRetiredModelEnv(environ []string) error {
	var set []string
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		retired := slices.Contains(retiredModelEnvNames, name) ||
			slices.ContainsFunc(retiredModelEnvPrefixes, func(p string) bool { return strings.HasPrefix(name, p) })
		if retired && !retiredModelEnvInert(value) {
			set = append(set, name)
		}
	}
	if len(set) == 0 {
		return nil
	}
	slices.Sort(set)
	return fmt.Errorf("refusing to start: %s: retired in 0.8.2, unset before starting — %s",
		strings.Join(set, ", "), api.RetiredModelCredentialRefusal)
}

// sweepRetiredModelCredentials deletes the retired operator-lane model
// credentials (api.RetiredModelCredentialNames) from every namespace, once per
// boot, and audits what it removed. Idempotent: the secrets API refuses to
// store those names again, so after the first boot of 0.8.2 there is nothing
// left to find. A store that cannot answer refuses boot rather than leave a
// retired credential in place.
func sweepRetiredModelCredentials(ctx context.Context, st secretstore.Store, rec audit.Recorder) error {
	names := api.RetiredModelCredentialNames()
	holders, err := st.Holders(ctx, names)
	if err != nil {
		return fmt.Errorf("refusing to start: list retired model credentials: %w", err)
	}
	if len(holders) == 0 {
		return nil
	}
	n, err := st.DeleteEverywhere(ctx, names)
	if err != nil {
		return fmt.Errorf("refusing to start: delete retired model credentials: %w", err)
	}
	data, _ := json.Marshal(map[string]any{
		"count": n,
		"names": slices.Sorted(maps.Keys(holders)),
	})
	ev := types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "wardynd",
		Action: "model_credential.retire", Target: st.Name(), Outcome: "success", Data: data,
	}
	if err := rec.Record(ctx, ev); err != nil {
		audit.LogWriteFailure(ctx, ev, err)
	}
	return nil
}
