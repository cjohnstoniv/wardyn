// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRedactSecretsMultilineValues(t *testing.T) {
	for _, value := range []string{
		"|-\n        first-sensitive-line\n        second-sensitive-line",
		">-\n        first-sensitive-line\n        second-sensitive-line",
		"\"first-sensitive-line\n        second-sensitive-line\"",
		"'first-sensitive-line\n        second-sensitive-line'",
	} {
		t.Run(value, func(t *testing.T) {
			input := "services:\n  app:\n    environment:\n      APP_PRIVATE_KEY: " + value + "\n      WARDYN_LISTEN: 127.0.0.1:8080\n"
			got := string(redactSecrets([]byte(input)))
			for _, secret := range []string{"first-sensitive-line", "second-sensitive-line"} {
				if strings.Contains(got, secret) {
					t.Errorf("secret survives in support bundle: %s", got)
				}
			}
			if !strings.Contains(got, "WARDYN_LISTEN: 127.0.0.1:8080") {
				t.Errorf("diagnostic sibling was dropped: %s", got)
			}
			var parsed any
			if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
				t.Errorf("redacted YAML is invalid: %v", err)
			}
		})
	}
}

func TestRedactSecretsAliasedMultilineValue(t *testing.T) {
	input := "x-value: &shared |\n  first-sensitive-line\n  second-sensitive-line\nservices:\n  app:\n    environment:\n      APP_PRIVATE_KEY: *shared\n      WARDYN_LISTEN: 127.0.0.1:8080\n"
	got := string(redactSecrets([]byte(input)))
	if strings.Contains(got, "sensitive-line") {
		t.Fatalf("anchor leaked a secret: %s", got)
	}
	var parsed any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("redacted YAML is invalid: %v", err)
	}
}

func TestRedactSecretsInvalidYAMLIsOmitted(t *testing.T) {
	for _, input := range []string{
		"APP_PRIVATE_KEY: \"first-sensitive-line\n  second-sensitive-line",
		"safe: yes\n---\nAPP_PRIVATE_KEY: sensitive-line\n",
	} {
		got := string(redactSecrets([]byte(input)))
		if strings.Contains(got, "sensitive-line") || !strings.Contains(got, "omitted") {
			t.Errorf("unparseable config must be omitted: %s", got)
		}
	}
}

func TestRedactSecretsMultilineEnvironmentList(t *testing.T) {
	input := "services:\n  app:\n    environment:\n      - |-\n        APP_PRIVATE_KEY=first-sensitive-line\n        second-sensitive-line\n      - WARDYN_LISTEN=127.0.0.1:8080\n"
	got := string(redactSecrets([]byte(input)))
	if strings.Contains(got, "sensitive-line") || !strings.Contains(got, "WARDYN_LISTEN=127.0.0.1:8080") {
		t.Fatalf("environment list redaction failed: %s", got)
	}
}

func TestRedactSecretsStructuredValues(t *testing.T) {
	cases := map[string]string{
		"aliased secret key":    "x-env-name: &env_name APP_PRIVATE_KEY\nservices:\n  app:\n    environment:\n      ? *env_name\n      : first-sensitive-line\n",
		"secret anchor":         "APP_PRIVATE_KEY: &shared |\n  first-sensitive-line\n  second-sensitive-line\ncopy: *shared\n",
		"secret mapping":        "credentials:\n  first: first-sensitive-line\n  second: second-sensitive-line\n",
		"folded embedded JSON":  "WARDYN_AUDIT_SINKS: >-\n  [{\"bearer_token\": \"first-sensitive-line\n  second-sensitive-line\"}]\n",
		"escaped embedded JSON": "WARDYN_AUDIT_SINKS: '{\"bearer_token\":\"first-sensitive-line\\\"second-sensitive-line\"}'\n",
		"commented multiline":   "# APP_PRIVATE_KEY: |\n#   first-sensitive-line\n#   second-sensitive-line\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			input += "diagnostic: kept\n"
			got := string(redactSecrets([]byte(input)))
			if strings.Contains(got, "sensitive-line") {
				t.Errorf("secret survived: %s", got)
			}
			if !strings.Contains(got, "diagnostic: kept") {
				t.Errorf("diagnostic sibling was lost: %s", got)
			}
			var parsed any
			if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
				t.Errorf("redacted YAML is invalid: %v", err)
			}
		})
	}
}
