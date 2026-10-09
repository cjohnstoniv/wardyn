// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package cliutil holds tiny env/flag helpers shared by Wardyn's cmd/* main
// packages, plus ScrubChildEnv, the one env denylist shared by every
// host-exec'd third-party CLI child.
package cliutil

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// exit is os.Exit, stubbed in tests. An unparseable env value is FATAL, not "fall back to default": a bad
// -flag value already exits 2, so the env door must not be quieter — a silently-reinterpreted typo is how
// a security toggle ends up silently off.
var exit = os.Exit

// envFatal reports an unusable env value and exits 2, mirroring a bad -flag value; written to
// flag.CommandLine's output so it lands with flag's own diagnostics.
func envFatal(env, val, want string) {
	fmt.Fprintf(flag.CommandLine.Output(), "invalid %s=%q: want %s\n", env, val, want)
	exit(2)
}

// EnvOr returns the env var if set and non-empty, else def.
func EnvOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// FlagEnv defines a string flag whose default is overridden by an env var. Unset/empty keeps the default;
// the escape hatch for a genuinely-intended blank is `-name=`.
//
// SECURITY: the env value is applied to the flag's VARIABLE, never its registered DEFAULT — PrintDefaults
// renders a non-empty default as `(default "…")` on every parse error, so seeding the default from the env
// would write secrets like WARDYN_AGE_KEY verbatim to stderr on one typo'd flag. Writing through the
// returned pointer keeps the same precedence while usage shows only the compiled default.
func FlagEnv(name, env, def, usage string) *string {
	p := flag.String(name, def, usage+" (env "+env+")")
	if v := os.Getenv(env); v != "" {
		*p = v
	}
	return p
}

// FlagBool defines a bool flag whose default is overridden by an env var. 1/true/yes/on is true,
// 0/false/no/off is false (case-insensitive, trimmed); unset/empty keeps the default. Anything else exits
// 2 — a value that's neither truthy nor falsey states no intent this helper can honor.
func FlagBool(name, env string, def bool, usage string) *bool {
	v := strings.TrimSpace(os.Getenv(env))
	switch strings.ToLower(v) {
	case "": // unset/empty: keep def, no noise
	case "1", "true", "yes", "on":
		def = true
	case "0", "false", "no", "off":
		def = false
	default:
		envFatal(env, v, "one of 1/true/yes/on or 0/false/no/off")
	}
	return flag.Bool(name, def, usage+" (env "+env+")")
}

// FlagDuration defines a time.Duration flag whose default is overridden by an env var. Unset/empty keeps
// the default; an unparseable value exits 2 rather than silently reinstating it.
func FlagDuration(name, env string, def time.Duration, usage string) *time.Duration {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			envFatal(env, v, "a Go duration such as 30s, 5m or 1h30m")
		} else {
			def = d
		}
	}
	return flag.Duration(name, def, usage+" (env "+env+")")
}

// FlagIntEnv defines an int flag whose default is overridden by an env var.
// Unset/empty keeps the default; an unparseable value exits 2 (see FlagDuration).
func FlagIntEnv(name, env string, def int, usage string) *int {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			envFatal(env, v, "an integer")
		} else {
			def = n
		}
	}
	return flag.Int(name, def, usage+" (env "+env+")")
}

// EnvBool reads a bool directly from an env var, no flag registered, for sites after flag.Parse(). Same
// token set and loudness contract as FlagBool.
func EnvBool(name string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(name))
	switch strings.ToLower(v) {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		envFatal(name, v, "one of 1/true/yes/on or 0/false/no/off")
		return def // unreachable in prod (envFatal exits); reached only under a stubbed exit in tests.
	}
}

// EnvDuration is the non-flag twin of FlagDuration for post-flag.Parse() sites.
func EnvDuration(name string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			envFatal(name, v, "a Go duration such as 30s, 5m or 1h30m")
			return def // unreachable in prod (envFatal exits); reached only under a stubbed exit in tests.
		}
		return d
	}
	return def
}

// ScrubChildEnv returns env, without mutating the input, minus variables a third-party CLI child must
// never receive: ANTHROPIC_API_KEY and every WARDYN_* (the daemon's own config, including secrets like
// WARDYN_AGE_KEY and WARDYN_ADMIN_TOKEN).
//
// Denylist, not allowlist: these children legitimately need whatever HTTPS_PROXY/NO_PROXY/AWS_* the shell
// carries, and a prefix covers every future WARDYN_* secret for free.
//
// SECURITY, HONEST RESIDUAL: defense-in-depth and consistency, NOT containment — a host-exec'd child runs
// as the same uid as wardynd and can read /proc/<ppid>/environ regardless. One documented exception
// (enforced by childenv_guard_test.go): `docker compose config` in supportbundle.go, whose output goes
// through redactSecrets first.
func ScrubChildEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") || strings.HasPrefix(kv, "WARDYN_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// SplitCSV splits a comma-separated list, trimming whitespace and dropping empties; nil for empty input
// (meaning "no restriction").
func SplitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
