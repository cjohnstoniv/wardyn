// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Planted rollbacks for rollbackguard's own test. It is under testdata, so it is never compiled.
package planted

import (
	"context"
	. "context"
	stdctx "context"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	xdb "github.com/cjohnstoniv/wardyn/internal/db"
)

func planted(ctx, rctx context.Context, tx pgx.Tx, s store) {
	defer tx.Rollback(context.WithoutCancel(ctx))                              // refused
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()             // refused
	_ = tx.Rollback(context.Background())                                      // refused
	tx.Rollback(context.TODO())                                                // refused
	_ = s.tx.Rollback(context.WithValue(context.WithoutCancel(ctx), key{}, 1)) // refused
	_ = tx.Rollback(                                                           // refused
		context.WithoutCancel(ctx),
	)
	defer tx.Rollback(ctx)                      // allowed: the caller's own context
	defer db.Rollback(ctx, tx)                  // allowed: the bounded form
	db.Rollback(context.WithoutCancel(ctx), tx) // allowed: the bounded form on a bound of its own
	_ = tx.Rollback(rctx)                       // allowed: a context with a bound of its own
}

// The file's own names for the context package: an alias and a dot import.
func plantedImportNames(ctx context.Context, tx pgx.Tx) {
	_ = tx.Rollback(stdctx.WithoutCancel(ctx)) // refused
	_ = tx.Rollback(Background())              // refused
}

// A detached context held in a name, passed on through another name, or bounded after.
func plantedNames(ctx context.Context, tx pgx.Tx) {
	dctx := context.WithoutCancel(ctx)
	_ = tx.Rollback(dctx) // refused
	var bg context.Context = context.Background()
	vctx := context.WithValue(bg, key{}, 1)
	_ = tx.Rollback(vctx) // refused
	cctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	_ = tx.Rollback(cctx) // refused
	bctx, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer stop()
	_ = tx.Rollback(bctx) // allowed: detached, then bounded
}

// The caller's ctx given a detached context.
func plantedReassigned(ctx context.Context, tx pgx.Tx) {
	ctx = context.Background()
	defer tx.Rollback(ctx) // refused
}

// A method value, and a call through parentheses.
func plantedMethodValue(ctx context.Context, tx pgx.Tx) {
	undo := tx.Rollback
	defer undo(context.WithoutCancel(ctx)) // refused
	defer undo(ctx)                        // allowed: the caller's own context
	(tx.Rollback)(context.Background())    // refused
}

// A method expression, and a method value passed on through a second name.
func plantedMethodExpression(ctx context.Context, tx pgx.Tx) {
	pgx.Tx.Rollback(tx, context.Background()) // refused
	pgx.Tx.Rollback(tx, ctx)                  // allowed: the caller's own context
	undo := tx.Rollback
	again := undo
	defer again(context.TODO()) // refused
}

// The bounded form over a transaction begun through a held context: the transaction is not the context.
func plantedBoundedForm(s store) {
	ctx := context.Background()
	tx, _ := s.conn(ctx).Begin(ctx)
	defer db.Rollback(ctx, tx) // allowed: the bounded form
}

// Functions of the package that answer a detached context (through another written after it and a named result,
// directly, and a method), one that keeps its caller's, and one that returns no context at all.
func detachAgain(ctx context.Context) (out context.Context) {
	out = detach(ctx)
	return
}

func detach(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

func (s store) background() context.Context { return context.Background() }

func keep(ctx context.Context) context.Context { return context.WithValue(ctx, key{}, 1) }

func done(ctx context.Context) error { return finish(context.WithoutCancel(ctx)) }

func plantedHelpers(ctx context.Context, tx pgx.Tx, s store) {
	_ = tx.Rollback(detachAgain(ctx))        // refused
	_ = tx.Rollback(detach(ctx))             // refused
	_ = tx.Rollback(s.background())          // refused
	_ = tx.Rollback(keep(ctx))               // allowed: a function that keeps its caller's context
	_ = tx.Rollback(withErr(ctx, done(ctx))) // allowed: done returns no context
}

// A function literal called where it is written, a literal that is only passed, and a name held out of source order.
func plantedLiterals(ctx context.Context, tx pgx.Tx) {
	_ = tx.Rollback(func() context.Context { return context.Background() }()) // refused
	_ = tx.Rollback(withHook(ctx, func() { _ = context.Background() }))       // allowed: the literal is not called here
	var a, b context.Context
	set := func() { a = b }
	b = context.Background()
	set()
	_ = tx.Rollback(a) // refused
}

// A range variable, a field that shares a held name, and a context bounded under the name it was detached with.
func plantedRangeFieldRebound(ctx context.Context, tx pgx.Tx, s store) {
	for _, c := range []context.Context{context.Background()} {
		_ = tx.Rollback(c) // refused
	}
	dctx := context.WithoutCancel(ctx)
	_ = tx.Rollback(dctx)   // refused
	_ = tx.Rollback(s.dctx) // allowed: a field, not the held name
	bg := context.Background()
	bg, cancel := context.WithTimeout(bg, time.Second)
	defer cancel()
	_ = tx.Rollback(bg) // refused: a name given a detached context stays held; the bounded one needs its own name
}

// A local that hides the db package: db.Rollback takes two arguments, so one argument is a method.
func plantedHiddenName(db pgx.Tx, s store) {
	_ = db.Rollback(context.Background()) // refused
	xdb, _ := s.conn(context.Background()).Begin(context.Background())
	_ = xdb.Rollback(context.Background()) // refused
}

// A method value taken from a local that hides the db package, and the same value passed on.
func plantedHiddenValue(db pgx.Tx) {
	undo := db.Rollback
	again := undo
	defer undo(context.Background()) // refused
	defer again(context.TODO())      // refused
}

// The bounded form's own value: it takes two arguments.
func plantedBoundedValue(s store) {
	ctx := context.Background()
	tx, _ := s.conn(ctx).Begin(ctx)
	undo := xdb.Rollback
	defer undo(ctx, tx) // allowed: the bounded form, through a name
}
