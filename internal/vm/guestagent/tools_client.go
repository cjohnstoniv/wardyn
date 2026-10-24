// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
)

// The host's side of tools mode (macos_tools.go): a Client whose dial is a session the guest opened on
// VsockToolsPort. The control session's Client answers none of these but ToolsDial.

// ToolsPush writes r, exactly size bytes whose SHA-256 is sha256Hex, to dest in the guest as root with
// mode's permission bits.
func (c *Client) ToolsPush(ctx context.Context, dest string, mode fs.FileMode, r io.Reader, size int64, sha256Hex string) error {
	q := url.Values{
		"dest":   {dest},
		"mode":   {strconv.FormatUint(uint64(mode.Perm()), 8)},
		"size":   {strconv.FormatInt(size, 10)},
		"sha256": {sha256Hex},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/tools/push?"+q.Encode(), r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	res, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return statusErr(res)
}

// ToolsRun runs argv in the guest as root and returns its combined output and exit code; err is a
// refusal or a transport failure, not a non-zero exit.
func (c *Client) ToolsRun(ctx context.Context, argv []string) (string, int, error) {
	var r ToolsExecResult
	if err := c.do(ctx, http.MethodPost, "/v1/tools/exec", ToolsExec{Argv: argv}, &r); err != nil {
		return "", 0, err
	}
	return r.Output, r.ExitCode, nil
}

// ToolsDesktopPassword sets Screen Sharing's VNC password and says whether it was set.
func (c *Client) ToolsDesktopPassword(ctx context.Context, pw string) (bool, error) {
	var r DesktopPasswordResult
	err := c.do(ctx, http.MethodPost, "/v1/tools/desktop_password", DesktopPassword{Password: pw}, &r)
	return r.Set, err
}

// ToolsPointer reads the pointer position in the console user's session.
func (c *Client) ToolsPointer(ctx context.Context) (Pointer, error) {
	var p Pointer
	return p, c.do(ctx, http.MethodGet, "/v1/tools/pointer", nil, &p)
}

// ToolsDial asks the control session how the agent's tools-port dial ended.
func (c *Client) ToolsDial(ctx context.Context) (ToolsDial, error) {
	var d ToolsDial
	return d, c.do(ctx, http.MethodGet, "/v1/dial/tools", nil, &d)
}
