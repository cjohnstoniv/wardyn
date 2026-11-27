// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	gotypes "go/types"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// wantZeroPortRefuses calls a zero port of one shape and fails unless it answers the zero T and
// errPauseInterfaceMissing.
func wantZeroPortRefuses[A, T any](t *testing.T, name string, a A) {
	t.Helper()
	var p movePort[A, T]
	got, err := p.call(context.Background(), a)
	if !errors.Is(err, errPauseInterfaceMissing) || !reflect.ValueOf(&got).Elem().IsZero() {
		t.Errorf("%s: an unset port answered %+v, %v; want the zero value and errPauseInterfaceMissing", name, got, err)
	}
}

// portShapes is each port's declared type, the shape TestMovePorts_ZeroPortRefuses calls a zero port of.
var portShapes = map[string]string{
	"movePortStopRefusal":       "movePort[types.AgentRun, string]",
	"movePortPauseGen":          "movePort[types.AgentRun, int64]",
	"movePortPauseStop":         "movePort[moveStopArgs, movePauseReceipt]",
	"movePortRevokeFor":         "movePort[moveGenArgs, moveRevokeState]",
	"movePortLineageCut":        "movePort[uuid.UUID, bool]",
	"movePortStampCutLineage":   "movePort[uuid.UUID, bool]",
	"movePortLocallyContained":  "movePort[uuid.UUID, bool]",
	"movePortMintAtDestination": "movePort[moveMintArgs, moveMintPlan]",
	"movePortCarriedSet":        "movePort[types.AgentRun, moveCarriedSet]",
	"movePortRevokeAtPause":     "movePort[moveGenArgs, moveRevokeState]",
	"movePortWithPauseResume":   "movePort[struct{}, context.Context]",
	"movePortResumeOn":          "movePort[moveResumeArgs, struct{}]",
}

// TestMovePorts_ZeroPortRefuses: an unset port of each port's shape refuses, and every port in run_move_ports.go is
// declared a movePort with no value.
func TestMovePorts_ZeroPortRefuses(t *testing.T) {
	run, id := types.AgentRun{ID: uuid.New()}, uuid.New()
	wantZeroPortRefuses[types.AgentRun, string](t, "movePortStopRefusal", run)
	wantZeroPortRefuses[types.AgentRun, int64](t, "movePortPauseGen", run)
	wantZeroPortRefuses[moveStopArgs, movePauseReceipt](t, "movePortPauseStop", moveStopArgs{Run: run})
	wantZeroPortRefuses[moveGenArgs, moveRevokeState](t, "movePortRevokeFor", moveGenArgs{RunID: id, Gen: 1})
	wantZeroPortRefuses[uuid.UUID, bool](t, "movePortLineageCut", id)
	wantZeroPortRefuses[uuid.UUID, bool](t, "movePortStampCutLineage", id)
	wantZeroPortRefuses[uuid.UUID, bool](t, "movePortLocallyContained", id)
	wantZeroPortRefuses[moveMintArgs, moveMintPlan](t, "movePortMintAtDestination", moveMintArgs{Run: run})
	wantZeroPortRefuses[types.AgentRun, moveCarriedSet](t, "movePortCarriedSet", run)
	wantZeroPortRefuses[moveGenArgs, moveRevokeState](t, "movePortRevokeAtPause", moveGenArgs{RunID: id, Gen: 1})
	wantZeroPortRefuses[struct{}, context.Context](t, "movePortWithPauseResume", struct{}{})
	wantZeroPortRefuses[moveResumeArgs, struct{}](t, "movePortResumeOn", moveResumeArgs{Run: run, Gen: 1})

	f, err := parser.ParseFile(token.NewFileSet(), "run_move_ports.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var declared []string
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, sp := range gd.Specs {
			vs := sp.(*ast.ValueSpec)
			for _, n := range vs.Names {
				if !moveSeamPort.MatchString(n.Name) {
					continue
				}
				declared = append(declared, n.Name)
				typ, ok := vs.Type.(*ast.IndexListExpr)
				if ok {
					base, isIdent := typ.X.(*ast.Ident)
					ok = isIdent && base.Name == "movePort" && len(typ.Indices) == 2
				}
				if !ok || len(vs.Values) != 0 {
					t.Errorf("%s is not declared a movePort[A, T] with no value", n.Name)
				} else if got := gotypes.ExprString(vs.Type); got != portShapes[n.Name] {
					t.Errorf("%s is %s, want %s, the shape this test calls", n.Name, got, portShapes[n.Name])
				}
			}
		}
	}
	slices.Sort(declared)
	want := slices.Sorted(maps.Keys(portShapes))
	if !slices.Equal(declared, want) {
		t.Errorf("run_move_ports.go declares %v, want the twelve ports %v", declared, want)
	}
}
