// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package spire

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	entryv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/server/entry/v1"
	svidv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/server/svid/v1"
	spiretypes "github.com/spiffe/spire-api-sdk/proto/spire/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"

	"github.com/cjohnstoniv/wardyn/internal/identity/svidsource"
)

// serverPath is the SPIRE Server's default SPIFFE ID path; wardynd authorises exactly that peer.
const serverPath = "/spire/server"

// grpcServer is Server over the SPIRE Server API, dialled with mTLS from wardynd's control-plane SVID.
type grpcServer struct {
	conn  *grpc.ClientConn
	entry entryv1.EntryClient
	svid  svidv1.SVIDClient
}

func dialGRPC(addr string, src svidsource.Source, td spiffeid.TrustDomain) (Server, error) {
	serverID, err := spiffeid.FromPath(td, serverPath)
	if err != nil {
		return nil, err
	}
	tlsConf := tlsconfig.MTLSClientConfig(src, src, tlsconfig.AuthorizeID(serverID))
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsConf)))
	if err != nil {
		return nil, fmt.Errorf("spire identity: dial %s=%s: %w", envServerAddress, addr, err)
	}
	return &grpcServer{conn: conn, entry: entryv1.NewEntryClient(conn), svid: svidv1.NewSVIDClient(conn)}, nil
}

func (g *grpcServer) Close() error { return g.conn.Close() }

func toProtoID(id spiffeid.ID) *spiretypes.SPIFFEID {
	return &spiretypes.SPIFFEID{TrustDomain: id.TrustDomain().Name(), Path: id.Path()}
}

func (g *grpcServer) MintJWTSVID(ctx context.Context, id spiffeid.ID, audience string, ttl time.Duration) (string, error) {
	resp, err := g.svid.MintJWTSVID(ctx, &svidv1.MintJWTSVIDRequest{
		Id: toProtoID(id), Audience: []string{audience}, Ttl: int32(ttl / time.Second),
	})
	if err != nil {
		return "", err
	}
	if resp.GetSvid().GetToken() == "" {
		return "", errors.New("spire identity: the server minted an empty JWT-SVID")
	}
	return resp.GetSvid().GetToken(), nil
}

func (g *grpcServer) FindEntries(ctx context.Context, sel Selector) ([]string, error) {
	entries, err := g.list(ctx, &entryv1.ListEntriesRequest_Filter{BySelectors: &spiretypes.SelectorMatch{
		Selectors: []*spiretypes.Selector{{Type: sel.Type, Value: sel.Value}},
		Match:     spiretypes.SelectorMatch_MATCH_SUPERSET,
	}})
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.GetId())
	}
	return ids, err
}

func (g *grpcServer) ListEntriesByID(ctx context.Context, id spiffeid.ID) ([]Entry, error) {
	entries, err := g.list(ctx, &entryv1.ListEntriesRequest_Filter{BySpiffeId: toProtoID(id)})
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		x := Entry{ID: e.GetId(), SPIFFEID: id, X509SVIDTTL: time.Duration(e.GetX509SvidTtl()) * time.Second}
		for _, s := range e.GetSelectors() {
			x.Selectors = append(x.Selectors, Selector{Type: s.GetType(), Value: s.GetValue()})
		}
		out = append(out, x)
	}
	return out, nil
}

func (g *grpcServer) list(ctx context.Context, f *entryv1.ListEntriesRequest_Filter) ([]*spiretypes.Entry, error) {
	var out []*spiretypes.Entry
	token := ""
	for {
		resp, err := g.entry.ListEntries(ctx, &entryv1.ListEntriesRequest{Filter: f, PageSize: 100, PageToken: token})
		if err != nil {
			return nil, err
		}
		out = append(out, resp.GetEntries()...)
		if token = resp.GetNextPageToken(); token == "" {
			return out, nil
		}
	}
}

func (g *grpcServer) CreateEntry(ctx context.Context, e Entry) error {
	pe := &spiretypes.Entry{SpiffeId: toProtoID(e.SPIFFEID), ParentId: toProtoID(e.ParentID),
		X509SvidTtl: int32(e.X509SVIDTTL / time.Second)}
	for _, s := range e.Selectors {
		pe.Selectors = append(pe.Selectors, &spiretypes.Selector{Type: s.Type, Value: s.Value})
	}
	resp, err := g.entry.BatchCreateEntry(ctx, &entryv1.BatchCreateEntryRequest{Entries: []*spiretypes.Entry{pe}})
	if err != nil {
		return err
	}
	for _, r := range resp.GetResults() {
		if c := codes.Code(r.GetStatus().GetCode()); c != codes.OK && c != codes.AlreadyExists {
			return fmt.Errorf("spire identity: create entry for %s: %s: %s", e.SPIFFEID, c, r.GetStatus().GetMessage())
		}
	}
	return nil
}

func (g *grpcServer) DeleteEntries(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	resp, err := g.entry.BatchDeleteEntry(ctx, &entryv1.BatchDeleteEntryRequest{Ids: ids})
	if err != nil {
		return err
	}
	for _, r := range resp.GetResults() {
		if c := codes.Code(r.GetStatus().GetCode()); c != codes.OK && c != codes.NotFound {
			return fmt.Errorf("spire identity: delete entry %s: %s: %s", r.GetId(), c, r.GetStatus().GetMessage())
		}
	}
	return nil
}
