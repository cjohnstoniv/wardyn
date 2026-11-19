// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "slices"

// ModelLimit is the run's governed model list as its proxy holds it: the models (proxy.Config.AllowedModels)
// and the Bedrock gateways a provider names by its own address (proxy.Config.BedrockRoutes). Nil when no
// limit binds the run; Models is never empty, since governance has no deny-all value.
type ModelLimit struct {
	Models []string `json:"models"`
	// BedrockRoutes maps a Bedrock gateway's host to the base path its Bedrock API is served under.
	BedrockRoutes map[string]string `json:"bedrock_routes,omitempty"`
}

// ModelLimitOf is the limit a stored proxy config carries: nil when it has no list.
func ModelLimitOf(models *[]string, routes map[string]string) *ModelLimit {
	if models == nil {
		return nil
	}
	return &ModelLimit{Models: slices.Clone(*models), BedrockRoutes: routes}
}

// proxyList is the list as proxy.Config carries it: nil, so no key is written, for no limit.
func (l *ModelLimit) proxyList() *[]string {
	if l == nil {
		return nil
	}
	list := slices.Clone(l.Models)
	return &list
}

// proxyRoutes is the Bedrock routes as proxy.Config carries them.
func (l *ModelLimit) proxyRoutes() map[string]string {
	if l == nil {
		return nil
	}
	return l.BedrockRoutes
}
