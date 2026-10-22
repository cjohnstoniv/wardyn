// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package foldersync is the engine of `wardyn sync`: one pass between a folder
// on a person's computer and a sandbox directory, over the sandbox's own
// sftp-server, under the safety rules in rules.go. The CLI runs it over the SSH
// gateway; a runner in folder-only mode runs it over its organisation session.
package foldersync
