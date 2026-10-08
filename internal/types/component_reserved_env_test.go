// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reservedEnvNames is the closed list, written out here on purpose: a name
// dropped from the validator's list has to be dropped from this one too.
var reservedEnvNames = []string{
	"PATH", "HOME", "SHELL", "BASH_ENV", "ENV", "PROMPT_COMMAND",
	"NODE_OPTIONS", "NODE_PATH", "PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP",
	"PERL5OPT", "PERL5LIB", "RUBYOPT", "RUBYLIB",
	"GIT_EXEC_PATH", "GIT_SSH", "GIT_SSH_COMMAND", "GIT_PROXY_COMMAND", "CLAUDE_CONFIG_DIR",
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
	"NODE_EXTRA_CA_CERTS", "GIT_SSL_CAINFO", "GIT_SSL_NO_VERIFY",
	// One and more of each reserved prefix.
	"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "LD_",
	"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_GLOBAL",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_ANYTHING",
}

// A component may not set a variable that decides how programs in the sandbox
// start or how they reach the network — as a config key or as the variable a
// secret is delivered in, on an organisation's row and on a person's alike.
// The refusal names the variable and says it is reserved. Component.Validate
// is what the save routes call, and ComponentRef.Validate what a run's inline
// component goes through.
func TestComponentDefinitionValidate_RefusesReservedEnvironmentNames(t *testing.T) {
	for _, name := range reservedEnvNames {
		asConfig := types.ComponentDefinition{Config: map[string]string{name: "v"}}
		asEnvVar := types.ComponentDefinition{Secrets: []types.ComponentSecret{env("token", name)}}
		for where, def := range map[string]types.ComponentDefinition{"config key": asConfig, "env variable": asEnvVar} {
			for _, personDefined := range []bool{true, false} {
				err := def.Validate(proxy.ValidDomainEntry, personDefined)
				if err == nil || !strings.Contains(err.Error(), `"`+name+`" is reserved`) {
					t.Errorf("%s %s (person-defined %v): Validate = %v, want a refusal naming it as reserved", where, name, personDefined, err)
				}
			}
			for _, owner := range []string{"", "sub-person"} {
				row := types.Component{ID: uuid.New(), Owner: owner, Name: "Tool", Definition: def}
				if err := row.Validate(proxy.ValidDomainEntry); err == nil || !strings.Contains(err.Error(), `"`+name+`" is reserved`) {
					t.Errorf("%s %s on a stored row (owner %q): Validate = %v, want a refusal", where, name, owner, err)
				}
			}
			ref := types.ComponentRef{Inline: &def}
			if err := ref.Validate(proxy.ValidDomainEntry); err == nil || !strings.Contains(err.Error(), `"`+name+`" is reserved`) {
				t.Errorf("%s %s inline: Validate = %v, want a refusal", where, name, err)
			}
		}
	}
}

// The list is closed: a name that only looks like a reserved one is an
// ordinary variable, and a file delivery's name is not a variable at all.
func TestComponentDefinitionValidate_ReservedEnvironmentNamesAreExact(t *testing.T) {
	ok := []string{
		"PATHS", "MY_PATH", "APP_HOME", "HOMEPAGE", "ENVIRONMENT", "NODE_ENV", "PYTHONUNBUFFERED",
		"LDFLAGS", "GIT_AUTHOR_NAME", "GIT_CONFIG", "CLAUDE_MODEL_HINT", "PROXY_URL", "SSL_MODE", "REGION",
	}
	config := map[string]string{}
	for _, name := range ok {
		config[name] = "v"
	}
	def := types.ComponentDefinition{Config: config, Secrets: []types.ComponentSecret{env("token", "SERVICE_TOKEN")}}
	for _, personDefined := range []bool{true, false} {
		if err := def.Validate(proxy.ValidDomainEntry, personDefined); err != nil {
			t.Errorf("person-defined %v: Validate = %v, want these ordinary names accepted", personDefined, err)
		}
	}
	if !types.ComponentFileDelivery {
		return
	}
	for _, file := range []string{"path", "ld_preload", "node_options", "home"} {
		byFile := types.ComponentDefinition{Secrets: []types.ComponentSecret{{SecretName: "token",
			Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryFile, File: file}}}}
		if err := byFile.Validate(proxy.ValidDomainEntry, true); err != nil {
			t.Errorf("file delivery named %q: Validate = %v, want it accepted (a file name is not a variable)", file, err)
		}
	}
}
