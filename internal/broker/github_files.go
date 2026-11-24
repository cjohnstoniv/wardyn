// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"fmt"
	"slices"

	gh "github.com/google/go-github/v88/github"
)

// File is one root file of a repository, as ReadRootFiles found it. Branch is the branch it was read from.
// A file over the byte cap is named with TooLarge set and no text.
type File struct {
	Name     string
	Branch   string
	Size     int
	Text     string
	TooLarge bool
}

// githubFilesTimeout bounds one read of a repository's root: the listing and each file, every call under its
// own client timeout.
const githubFilesTimeout = 4 * githubClientTimeout

// ReadRootFiles reads the files of repo's ("owner/name") root that names lists, at the default branch, in the
// order names gives them. Only an entry of type file counts: a symlink, a submodule or a directory with a
// listed name is left alone. A file of more than maxBytes comes back named with TooLarge and no text.
//
// It mints its own token as ListBranches does: one repository, contents:read and nothing else, handed back
// once the read is done. The token never leaves this function.
func (m *githubMinter) ReadRootFiles(ctx context.Context, repo string, names []string, maxBytes int) ([]File, error) {
	owner, rnames, err := splitRepos([]string{repo})
	if err != nil {
		return nil, err
	}
	tok, _, err := m.MintInstallationToken(ctx, []string{repo}, map[string]string{"contents": "read"}, defaultMaxTTL)
	if err != nil {
		return nil, githubRefusal(err)
	}
	defer func() { _ = m.Revoke(ctx, tok) }()
	ctx, cancel := context.WithTimeout(ctx, githubFilesTimeout)
	defer cancel()
	opts := []gh.ClientOptionsFunc{gh.WithAuthToken(tok), gh.WithTimeout(m.timeout())}
	if m.baseURL != "" {
		opts = append(opts, gh.WithURLs(&m.baseURL, nil))
	}
	c, err := gh.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("broker: build github client: %w", err)
	}
	r, _, err := c.Repositories.Get(ctx, owner, rnames[0])
	if err != nil {
		return nil, githubRefusal(err)
	}
	branch := r.GetDefaultBranch()
	at := &gh.RepositoryContentGetOptions{Ref: branch}
	_, root, _, err := c.Repositories.GetContents(ctx, owner, rnames[0], "", at)
	if err != nil {
		return nil, githubRefusal(err)
	}
	found := map[string]*gh.RepositoryContent{}
	for _, e := range root {
		if e.GetType() == "file" && slices.Contains(names, e.GetName()) {
			found[e.GetName()] = e
		}
	}
	var out []File
	for _, name := range names {
		e, ok := found[name]
		if !ok {
			continue
		}
		f := File{Name: name, Branch: branch, Size: e.GetSize()}
		if f.Size > maxBytes {
			f.TooLarge = true
			out = append(out, f)
			continue
		}
		one, _, _, err := c.Repositories.GetContents(ctx, owner, rnames[0], name, at)
		if err != nil {
			return nil, githubRefusal(err)
		}
		if one == nil || one.GetType() != "file" {
			continue
		}
		text, err := one.GetContent()
		if err != nil {
			return nil, fmt.Errorf("broker: decode %s: %w", name, err)
		}
		if f.Size = len(text); f.Size > maxBytes {
			f.TooLarge = true
		} else {
			f.Text = text
		}
		out = append(out, f)
	}
	return out, nil
}
