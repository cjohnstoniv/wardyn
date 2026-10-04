// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import "net/url"

// ChannelInfo is what the console may show about a configured channel: its operator-chosen id, its
// type and the destination HOST. Never the URL: a chat webhook carries its credential in the path
// or query, and a URL may carry userinfo.
type ChannelInfo struct {
	ID, Type, Host string
}

// ChannelInfos lists the configured channels in config order, or nil when notifications are off.
func ChannelInfos() []ChannelInfo {
	c := active.Load()
	if c == nil {
		return nil
	}
	out := make([]ChannelInfo, len(c.Channels))
	for i := range c.Channels {
		out[i] = ChannelInfo{ID: c.Channels[i].ID, Type: c.Channels[i].Type, Host: c.Channels[i].host()}
	}
	return out
}

// host is the hostname of the channel's URL, parsed, never cut out of the string. An unparsable URL
// yields "": Parse already refused one at boot, so this is only a guard.
func (ch *Channel) host() string {
	u, err := url.Parse(ch.URL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
