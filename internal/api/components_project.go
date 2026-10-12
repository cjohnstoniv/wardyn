// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// orgComponentView is what a person granted an org component sees of it: where
// it reaches and how each secret is delivered, so they know what their run will
// be able to do. Not the secret names, not the config values, and not the owner —
// an org row's content is admin-authored and these are the parts that never
// needed to leave it.
func orgComponentView(c types.Component) client.OrgComponentView {
	v := client.OrgComponentView{
		ID: c.ID, Name: c.Name,
		Hosts:      append([]string{}, c.Definition.Hosts...),
		Secrets:    make([]client.ComponentSecretView, 0, len(c.Definition.Secrets)),
		ConfigKeys: sortedKeys(c.Definition.Config),
	}
	for _, sec := range c.Definition.Secrets {
		v.Secrets = append(v.Secrets, client.ComponentSecretView{Delivery: sec.Delivery, Shared: sec.Shared})
	}
	v.Category = "api"
	if cli := c.Definition.CLI; cli != nil {
		v.Category, v.CLI = "cli", cliView(cli)
	}
	return v
}

// cliView is the non-secret face of a CLI block: no portal URL, no subscription ids.
func cliView(c *types.ComponentCLI) *client.CLIView {
	v := &client.CLIView{Kind: c.Kind}
	switch {
	case c.Azure != nil:
		v.Subscriptions, v.Access = len(c.Azure.Subscriptions), c.Azure.Access
	case c.AWS != nil:
		v.AccountID, v.RoleName, v.Region = c.AWS.AccountID, c.AWS.RoleName, c.AWS.Region
	case c.GitHub != nil:
		v.Capabilities = append([]ghscope.Capability{}, c.GitHub.Capabilities...)
	}
	return v
}

// catalogueCLIs is the five CLI cards in their fixed order. It reads only the
// rows the caller is granted, so it cannot say anything about an ungranted one.
func catalogueCLIs(granted []client.OrgComponentView) []client.CatalogueCLI {
	cards := []client.CatalogueCLI{
		{Kind: types.CLIKindAzure}, {Kind: types.CLIKindGitHub}, {Kind: types.CLIKindAWS},
		{Kind: "gcloud", State: client.CLIStateComingSoon}, {Kind: "kubectl", State: client.CLIStateComingSoon},
	}
	for i := range cards {
		cards[i].Rows = []uuid.UUID{}
		if cards[i].State == "" {
			cards[i].State = client.CLIStateNotGranted
		}
		for _, row := range granted {
			if row.CLI != nil && row.CLI.Kind == cards[i].Kind {
				cards[i].State = client.CLIStateAvailable
				cards[i].Rows = append(cards[i].Rows, row.ID)
			}
		}
	}
	return cards
}

// handleMyComponents is GET /me/components: what the caller may do, the rows they
// saved, and the org rows they are granted. An org row the caller is not granted
// is absent, asked through the same door a run's attach asks, so the list and the
// refusal can never disagree about who may use a row.
//
// The rows are read BEFORE the first capability question. The answers are
// memoised for the request, and a new org row commits together with its
// restriction: a memo loaded after the rows were listed already holds the
// restriction of every row in the list, where one loaded before would judge a
// row created in between as unrestricted and show it to everyone.
func (s *Server) handleMyComponents(w http.ResponseWriter, r *http.Request) {
	cs, ok := s.componentStoreOr501(w)
	if !ok {
		return
	}
	owner, ok := s.componentOwner(w, r)
	if !ok {
		return
	}
	// One capability snapshot for the whole listing.
	r = r.WithContext(withCapBatch(r.Context()))
	ctx := r.Context()
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		writeServerError(w, r, "read site config", err)
		return
	}
	settings := componentSettings(sc)
	mine, err := cs.ListComponents(ctx, owner)
	if err != nil {
		writeServerError(w, r, "list components", err)
		return
	}
	if mine == nil {
		mine = []types.Component{}
	}
	orgRows, err := cs.ListComponents(ctx, "")
	if err != nil {
		writeServerError(w, r, "list components", err)
		return
	}
	define := s.componentAttachRefusal(r, "")
	if define != nil && define.err != nil {
		define.write(s, w, r)
		return
	}
	org := []client.OrgComponentView{}
	for _, c := range orgRows {
		ref := s.componentAttachRefusal(r, c.ID.String())
		if ref != nil && ref.err != nil {
			ref.write(s, w, r)
			return
		}
		if ref == nil {
			org = append(org, orgComponentView(c))
		}
	}
	writeJSON(w, http.StatusOK, client.MyComponents{
		MayDefine:               define == nil,
		ResidentDeliveryAllowed: !settings.DenyResidentDelivery,
		AutonomyCap:             settings.AutonomyCap,
		Mine:                    mine,
		Org:                     org,
		CLIs:                    catalogueCLIs(org),
	})
}
