// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package cliutil holds tiny env/flag helpers shared by Wardyn's cmd/* main
// packages (each cmd is its own `main` package, so these can't just live in
// one of them without the others importing "main").
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
// door must not be quieter than the flag door. Silently reinterpreting an
// unparseable value as the default is how `WARDYN_ENVBUILD=treu` turns a
// feature OFF with no error, no warning, and no log line — the operator only
// finds out when the feature they thought they enabled never runs. Failing
// closed at startup is cheap; a security toggle silently off is not.
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

// FlagEnv defines a string flag whose default is overridden by an env var.
func FlagEnv(name, env, def, usage string) *string {
	if v, ok := os.LookupEnv(env); ok {
		def = v
	}
	return flag.String(name, def, usage+" (env "+env+")")
}

// FlagBool defines a bool flag whose default is overridden by an env var.
// 1/true/yes/on is true, 0/false/no/off is false (case-insensitive, trimmed).
// Unset — or set to the empty string, which is what `docker run -e VAR` and a
// compose `VAR=` passthrough produce for an unset var — means "use the
// default", silently. Anything else exits 2: a value that is neither truthy nor
// falsey states no intent this helper can honor, and guessing "false" is the
// worst guess available (it turns features off).
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
// than silently reinstating the default — an operator who typos an interval
// must not have it quietly reinterpreted as a different, meaningful setting.
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
