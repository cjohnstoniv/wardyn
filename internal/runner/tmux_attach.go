// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "strings"

// TmuxAttachCommands is the tmux server configuration applied at attach, one
// command per entry, in the order of the non-comment lines of
// deploy/images/common/tmux.conf (a parity test enforces both). A sandbox
// started from an older image, or a BYOI image, never read the current
// tmux.conf; chaining these after new-session gives it the same settings on
// its next attach. They are a UX default the sandbox can override; nothing
// security-relevant depends on them. Session-local overrides (set without -g)
// shadow the global values and are left alone on purpose.
var TmuxAttachCommands = []string{
	`set -g status off`,
	`set -g mouse on`,
	`unbind -n MouseDown3Pane`,
	`unbind -n M-MouseDown3Pane`,
	`set -g set-clipboard external`,
	`set -g focus-events on`,
	`bind -n WheelUpPane if -F '#{||:#{mouse_any_flag},#{pane_in_mode}}' 'send-keys -M' 'if -F "#{alternate_on}" "send-keys Up" "copy-mode -e; send-keys -M"'`,
	`bind -n WheelDownPane if -F '#{||:#{mouse_any_flag},#{pane_in_mode}}' 'send-keys -M' 'if -F "#{alternate_on}" "send-keys Down" "send-keys -M"'`,
	`set -g history-limit 50000`,
	`set -s escape-time 0`,
	`set -g default-terminal "screen-256color"`,
	`set -ga terminal-overrides ",*:Tc"`,
	`set -g allow-rename off`,
	`setw -g automatic-rename off`,
}

// TmuxAttachSh is the shell fragment both drivers run for the tmux branch of
// attach: `tmux -V` is read with a bounded read and matched by a strict
// pattern, and the settings chain is added only for tmux >= 3.2. On any
// mismatch (unexpected output, older tmux) the attach proceeds exactly as
// before. The version text comes from inside the sandbox and is untrusted.
var TmuxAttachSh = tmuxAttachSh("")

// TmuxObserverAttachSh is TmuxAttachSh for an observer: inside the same
// tmux >= 3.2 branch the client is attached with `-f ignore-size`, so tmux
// never counts it when sizing the shared window. Older tmux attaches exactly
// as TmuxAttachSh does (the caller seeds the observer's PTY from the writer).
var TmuxObserverAttachSh = tmuxAttachSh(" -f ignore-size")

// TmuxAttachShFor returns the fragment for one role.
func TmuxAttachShFor(observer bool) string {
	if observer {
		return TmuxObserverAttachSh
	}
	return TmuxAttachSh
}

func tmuxAttachSh(flags string) string {
	return `v=$(tmux -V 2>/dev/null | head -c 64 | sed -n '1s/^tmux \([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\1 \2/p'); set -- $v; ` +
		`if [ $# -eq 2 ] && { [ "$1" -gt 3 ] 2>/dev/null || { [ "$1" -eq 3 ] && [ "$2" -ge 2 ]; } 2>/dev/null; }; ` +
		`then exec tmux new-session -A` + flags + ` -s wardyn bash \; ` + strings.Join(TmuxAttachCommands, ` \; `) + `; ` +
		`else exec tmux new-session -A -s wardyn bash; fi`
}
