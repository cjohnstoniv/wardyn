// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

// refuseHostKeyCollisions refuses two entries of one config table that name one host (hostKey, or a local
// model's host and port). A model table keys by that and keeps one of them, in map order; the git PAT
// tables key by spelling and hold both, so a request is held to whichever entry its spelling finds. Each
// pair is named as written. A via_org route is a list, read first to last, so two routes for one host are
// refused only when they name two grants.
func (c *Config) refuseHostKeyCollisions() error {
	var errs []error
	collide := func(field, what string, entries []string, key func(string) (string, bool)) {
		seen := map[string]string{}
		for _, e := range slices.Sorted(slices.Values(entries)) {
			k, ok := key(e)
			if !ok {
				continue
			}
			if first, dup := seen[k]; dup {
				errs = append(errs, fmt.Errorf("config: %s: %q and %q name one %s; keep one", field, first, e, what))
				continue
			}
			seen[k] = e
		}
	}
	host := func(e string) (string, bool) { return hostKey(e), true }
	collide("pat_grants", "host", slices.Collect(maps.Keys(c.PATGrants)), host)
	collide("git_push_withheld", "host", slices.Collect(maps.Keys(c.GitPushWithheld)), host)
	collide("llm_channel_hosts", "host", slices.Collect(maps.Keys(c.LLMChannelHosts)), host)
	collide("llm_headers", "host", slices.Collect(maps.Keys(c.LLMHeaders)), host)
	collide("bedrock_routes", "host", slices.Collect(maps.Keys(c.BedrockRoutes)), host)
	collide("local_models", "host and port", slices.Collect(maps.Keys(c.LocalModels)), func(raw string) (string, bool) {
		k, _, ok := localModelURLKey(raw)
		return k, ok
	})
	for j, rt := range c.ViaOrgRoutes {
		for i, o := range c.ViaOrgRoutes[:j] {
			if hostKey(o.Host) == hostKey(rt.Host) && o.GrantID != rt.GrantID {
				errs = append(errs, fmt.Errorf("config: via_org_routes[%d].host %q and via_org_routes[%d].host %q name one host for two grants; keep one", i, o.Host, j, rt.Host))
			}
		}
	}
	return errors.Join(errs...)
}
