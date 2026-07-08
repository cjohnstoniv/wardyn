// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package cliutil holds tiny env/flag helpers shared by Wardyn's cmd/* main
// packages (each cmd is its own `main` package, so these can't just live in
// one of them without the others importing "main").
package cliutil

import (
	"flag"
	"os"
	"strconv"
	"strings"
	"time"
)

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
// Any of 1/true/yes/on (case-insensitive) is true; anything else is false.
func FlagBool(name, env string, def bool, usage string) *bool {
	if v, ok := os.LookupEnv(env); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			def = true
		default:
			def = false
		}
	}
	return flag.Bool(name, def, usage+" (env "+env+")")
}

// FlagDuration defines a time.Duration flag whose default is overridden by an
// env var. An unparseable env value keeps the default (fail safe to default).
func FlagDuration(name, env string, def time.Duration, usage string) *time.Duration {
	if v, ok := os.LookupEnv(env); ok {
		if d, err := time.ParseDuration(v); err == nil {
			def = d
		}
	}
	return flag.Duration(name, def, usage+" (env "+env+")")
}

// FlagIntEnv defines an int flag whose default is overridden by an env var.
// An unparseable env value keeps the default (fail safe to default).
func FlagIntEnv(name, env string, def int, usage string) *int {
	if v, ok := os.LookupEnv(env); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
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
