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

// exit is os.Exit, stubbed in tests. An env value the helper cannot parse is
// FATAL, not "fall back to the default": the same setting arriving as a bad
// -flag value already exits 2 (flag.CommandLine is ExitOnError), so the env
// door must not be quieter than the flag door — a silently-reinterpreted typo
// is how a security toggle ends up silently off.
var exit = os.Exit

// envFatal reports an unusable env value and exits 2, mirroring how the flag
// package rejects a bad -flag value. Written to flag.CommandLine's output
// (os.Stderr by default) so it lands with flag's own diagnostics.
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

// EnvAlias lets a deprecated env var name go on working for one deprecation
// window while a new name takes over (UT-5, user-types-design.md rev 4 §6).
// Empty counts as unset for both names.
//
// When newEnv is unset and oldEnv is set, it copies oldEnv's value into newEnv
// and reports aliased. When both are set, newEnv wins; if the values differ it
// reports ignored, since a dropped old value can be a longer deny list the
// operator still believes is in force. Callers must run it before ANY flag is
// parsed or either name is otherwise read.
func EnvAlias(newEnv, oldEnv string) (aliased, ignored bool) {
	oldV := os.Getenv(oldEnv)
	if oldV == "" {
		return false, false
	}
	if newV := os.Getenv(newEnv); newV != "" {
		return false, newV != oldV
	}
	os.Setenv(newEnv, oldV) //nolint:errcheck // this process's own env; Setenv cannot fail here
	return true, false
}

// FlagEnv defines a string flag whose default is overridden by an env var.
// Unset/empty means "use the default", like FlagBool/FlagDuration/FlagIntEnv/
// EnvOr; the escape hatch for a genuinely-intended blank is `-name=`.
//
// SECURITY: the env value is applied to the flag's VARIABLE, never to its
// registered DEFAULT — PrintDefaults renders a non-empty string default as
// `(default "…")` on every parse error, so seeding the default from the env
// would write secrets like WARDYN_AGE_KEY verbatim to stderr on one typo'd
// flag. Writing through the returned pointer instead keeps the same
// precedence while the usage block shows only the compiled default.
func FlagEnv(name, env, def, usage string) *string {
	p := flag.String(name, def, usage+" (env "+env+")")
	if v := os.Getenv(env); v != "" {
		*p = v
	}
	return p
}

// FlagBool defines a bool flag whose default is overridden by an env var.
// 1/true/yes/on is true, 0/false/no/off is false (case-insensitive, trimmed).
// Unset/empty means "use the default", silently. Anything else exits 2: a
// value that is neither truthy nor falsey states no intent this helper can
// honor, and guessing "false" is the worst guess available.
func FlagBool(name, env string, def bool, usage string) *bool {
	v := strings.TrimSpace(os.Getenv(env))
	switch strings.ToLower(v) {
	case "":
		// unset (or explicitly empty): keep def, no noise.
	case "1", "true", "yes", "on":
		def = true
	case "0", "false", "no", "off":
		def = false
	default:
		envFatal(env, v, "one of 1/true/yes/on or 0/false/no/off")
	}
	return flag.Bool(name, def, usage+" (env "+env+")")
}

// FlagDuration defines a time.Duration flag whose default is overridden by an
// env var. Unset/empty keeps the default; an unparseable value exits 2 rather
// than silently reinstating it.
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

// EnvBool reads a bool directly from an env var, with no flag registered — for
// sites where flag.Parse() has already run before the read. Same token set and
// loudness contract as FlagBool.
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

// EnvDuration reads a time.Duration directly from an env var, with no flag
// registered — the non-flag twin of FlagDuration for post-flag.Parse() sites.
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

// ScrubChildEnv returns env with the variables a third-party CLI child must
// never receive removed, without mutating the input: ANTHROPIC_API_KEY and
// every WARDYN_* variable (the daemon's own configuration, including secrets
// like WARDYN_AGE_KEY and WARDYN_ADMIN_TOKEN).
//
// Denylist, not allowlist: these children legitimately need whatever
// HTTPS_PROXY / NO_PROXY / AWS_* the shell carries, and a prefix covers every
// future WARDYN_* secret for free.
//
// HONEST RESIDUAL: defense-in-depth and consistency, NOT containment — a
// host-exec'd child runs as the same uid as wardynd and can read
// /proc/<ppid>/environ regardless. One documented exception (enforced by
// cmd/wardyn's childenv_guard_test.go): `docker compose config` in
// supportbundle.go, whose output goes through redactSecrets before it reaches
// a bundle.
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

// SplitCSV splits a comma-separated list, trimming whitespace and dropping
// empties. Returns nil for an empty input (meaning "no restriction").
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
