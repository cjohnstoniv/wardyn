// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// THE CENSUS THAT MAKES THE ISOLATION CLAIM ENFORCEABLE.
//
// Audit-chain link correctness rests on the isolation level of the transaction
// doing the writing: since 0056 the head read that decides prev_hash runs inside
// the inserting transaction, so a writer at REPEATABLE READ chains onto a head
// from before the previous writer committed and the verify sweep latches a
// permanent false tamper verdict. The level otherwise comes from
// default_transaction_isolation, a USERSET GUC any role can set per role or per
// database with no superuser involved.
//
// The tree answers that by PINNING the level on each audit-writing transaction,
// which overrides the GUC. The problem this file exists for is that the pin was
// asserted in prose long before it was true of every writer: db.go's boot ERROR
// told operators "Wardyn's own audit writers pin READ COMMITTED per transaction
// and are unaffected" while the broker's mint transaction was still on a bare
// Begin and still forked the chain — a false statement in an operator-facing log
// line, shipped by the same commit that wrote it, because nothing in the tree
// could contradict it.
//
// So: every transaction opened anywhere in the non-test tree is enumerated here
// and must be either PINNED or explicitly DECLARED with a reason. A new Begin
// reddens this test until somebody classifies it, and a declared site that
// disappears reddens it too, so the list cannot rot into a list of excuses for
// code that no longer exists. Classification is by AST, not by grepping for a
// symbol name: the sibling guard for the login re-stamp was defeated by a
// refactor that merely moved its symbol into a comment.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// txSite is one Begin/BeginTx call: where it is, and what encloses it.
type txSite struct {
	rel  string // repo-relative file
	fn   string // enclosing function (Recv.Method for methods)
	call string // the call's source text
	body string // the enclosing function's source text
}

func (s txSite) key() string { return s.rel + ":" + s.fn }

// pinned reports whether this site fixes its own isolation level, by either of
// the two forms the tree uses: pgx.TxOptions{IsoLevel: pgx.ReadCommitted} on a
// BeginTx, or `SET TRANSACTION ISOLATION LEVEL READ COMMITTED` as the
// transaction's first statement (db.beginReadCommitted, whose executor interface
// deliberately carries no BeginTx).
func (s txSite) pinned() bool {
	if strings.Contains(s.call, "ReadCommitted") {
		return true
	}
	return strings.Contains(s.body, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED")
}

// auditWriterPins are the transactions that WRITE audit_events. Each one must be
// pinned by the check above — this is the set db.go's operator-facing ERROR line
// makes a claim about, so it is named rather than derived: deriving "who writes
// audit_events" from the same source the claim is about would let both move
// together and prove nothing.
var auditWriterPins = map[string]string{
	"internal/store/store.go:InsertAuditEvent":           "the control plane's audit writer: pool.BeginTx(pgx.ReadCommitted), then the chain lock, then the INSERT",
	"internal/broker/pgx.go:PgxStore.BeginReadCommitted": "the broker's mint transaction: broker.mint opens it through the TxBeginner interface's ONLY opener, BeginReadCommitted (R3 HANDOFF-1), whose first statement is SET TRANSACTION ISOLATION LEVEL READ COMMITTED with a fail-closed rollback; insertAuditEventTx chains the mint's audit row inside it. The interface carries no bare Begin any more, so every caller of the seam is pinned by construction",
	"internal/db/db.go:beginReadCommitted":               "the boot chain canary, which INSERTs a synthetic audit row and rolls it back",
}

// declaredNonAuditTx are the transactions that do NOT write audit_events, each
// with the reason it cannot. Adding an entry here is a deliberate, reviewable
// act; the alternative — an unlisted bare Begin — is what shipped the false
// claim in the first place.
var declaredNonAuditTx = map[string]string{
	"internal/db/db.go:applyMigration": "runs one migration's DDL and records it in schema_migrations; it never inserts " +
		"into audit_events, and migration DDL is not chain-linked",
}

func TestEveryAuditWritingTransactionPinsReadCommitted(t *testing.T) {
	sites := collectTxSites(t)
	if len(sites) == 0 {
		t.Fatal("the census found no Begin/BeginTx call in the tree at all; this guard is scanning the wrong thing")
	}

	seen := map[string]bool{}
	for _, s := range sites {
		seen[s.key()] = true
		switch {
		case s.pinned():
			// A pinned site needs no declaration, whatever it writes.
		case declaredNonAuditTx[s.key()] != "":
			// Declared, with a reason, as unable to reach audit_events.
		default:
			t.Errorf("%s opens a transaction that neither pins READ COMMITTED nor is declared unable to write "+
				"audit_events.\n\tIf it can reach audit_events, pin it: BeginTx(ctx, pgx.TxOptions{IsoLevel: "+
				"pgx.ReadCommitted}) — at REPEATABLE READ it chains onto a stale head and the verify sweep latches a "+
				"permanent false tamper verdict.\n\tIf it cannot, add it to declaredNonAuditTx with the reason. What "+
				"it must not be is unclassified: db.go's boot ERROR tells operators every Wardyn audit writer is "+
				"pinned, and nothing but this census makes that true.", s.key())
		}
	}

	// The named audit writers must be pinned BY THE CHECK, never merely present.
	for key, why := range auditWriterPins {
		if !seen[key] {
			t.Errorf("%s is gone or renamed (%s). It is one of the transactions db.go's boot ERROR line makes a "+
				"claim about — re-derive this census against its new shape rather than deleting the entry", key, why)
			continue
		}
		if declaredNonAuditTx[key] != "" {
			t.Errorf("%s is listed in BOTH auditWriterPins and declaredNonAuditTx; it writes audit_events, so it "+
				"cannot be excused from the pin", key)
		}
	}
	for _, s := range sites {
		why, isWriter := auditWriterPins[s.key()]
		if isWriter && !s.pinned() {
			t.Errorf("%s NO LONGER PINS READ COMMITTED, and it writes audit_events (%s). At REPEATABLE READ it "+
				"chains onto a head from before the previous writer committed: two rows claim one predecessor and "+
				"store.VerifyAuditChain reports 'a row was deleted or reordered' over a log nobody touched — "+
				"permanently, per the latch. db.go's boot ERROR currently tells operators this transaction is "+
				"pinned; unpinning it makes that line false.", s.key(), why)
		}
	}

	// A declaration for code that no longer exists is a reason nobody can check.
	for key := range declaredNonAuditTx {
		if !seen[key] {
			t.Errorf("declaredNonAuditTx still excuses %s, which no longer opens a transaction; drop the entry so "+
				"this list stays a description of the tree rather than a list of excuses", key)
		}
	}
}

// collectTxSites parses every non-test .go file under internal/, cmd/ and pkg/
// and returns each call to a .Begin(...) or .BeginTx(...) method.
func collectTxSites(t *testing.T) []txSite {
	t.Helper()
	root := repoRoot(t)
	var out []txSite
	for _, top := range []string{"internal", "cmd", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			text := func(n ast.Node) string {
				return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || (sel.Sel.Name != "Begin" && sel.Sel.Name != "BeginTx") {
						return true
					}
					out = append(out, txSite{rel: rel, fn: funcName(fn), call: text(call), body: text(fn.Body)})
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// funcName renders a declaration as Name or Receiver.Name, matching the keys
// above.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}
