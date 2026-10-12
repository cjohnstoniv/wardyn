// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Run templates (migration 0165_run_templates): the catalogue row, its
// immutable revisions, the group-management grants and the group labels. A
// document is validated at the API boundary and stored as the door returned
// it. See TemplateStore (iface.go).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The most templates one audience holds: each person's own, each group's, and
// the organisation's. Counted and inserted under templateAudienceLockSQL, so
// two creates racing for the last slot cannot both land.
const (
	TemplatesMaxPerPerson = 200
	TemplatesMaxPerGroup  = 200
	TemplatesMaxOrg       = 500
)

// TemplateLimit is the most templates owner's audience may hold.
func TemplateLimit(owner types.TemplateOwner) int {
	switch owner.Scope {
	case types.TemplateScopeOrg:
		return TemplatesMaxOrg
	case types.TemplateScopeGroup:
		return TemplatesMaxPerGroup
	}
	return TemplatesMaxPerPerson
}

var (
	// ErrTemplateNameTaken: another template of the same audience has the name,
	// compared case-insensitively.
	ErrTemplateNameTaken = errors.New("store: a template available to the same people already uses this name")
	// ErrTemplateGrantGone: a group write found no grant for the writer when it
	// re-read it inside its own transaction.
	ErrTemplateGrantGone = errors.New("store: the template group grant is gone")
)

// TemplateLimitError: the audience already holds Limit templates.
type TemplateLimitError struct{ Limit int }

func (e *TemplateLimitError) Error() string {
	return "store: the template audience already holds " + strconv.Itoa(e.Limit) + " templates"
}

// TemplateRevisionConflictError: the template is at Current, not the revision
// the update expected. Nothing was written.
type TemplateRevisionConflictError struct{ Current int }

func (e *TemplateRevisionConflictError) Error() string {
	return "store: the template is at revision " + strconv.Itoa(e.Current)
}

// TemplateGrantCheck asks a group write to re-read the writer's grant inside
// its own transaction. Persons are every subject the writer is known by (an
// administrator may have granted either their sub or their email).
type TemplateGrantCheck struct {
	Group   string
	Persons []string
}

// RunSetupsErased is what EraseRunSetupsOf removed and cleared.
type RunSetupsErased struct {
	Templates         int `json:"templates"`
	Grants            int `json:"grants"`
	AuthorshipCleared int `json:"authorship_cleared"`
	// LaunchIntents is set by the caller that also erases the person's launch intents (LaunchIntentStore).
	LaunchIntents int `json:"launch_intents"`
}

// templateAudienceLockSQL is the named lock of D25-B: a transaction-scoped
// advisory lock keyed to one audience (scope, person, group). A create counts
// the audience and inserts while holding it, so the count it checks is the
// limit it enforces.
const templateAudienceLockSQL = `SELECT pg_advisory_xact_lock(hashtext($1))`

func templateAudienceKey(o types.TemplateOwner) string {
	return "run_templates.audience\x1f" + string(o.Scope) + "\x1f" + o.Person + "\x1f" + o.Group
}

// templateGrantForShareSQL re-reads the writer's grant and holds it until the
// write commits. A revoke (DELETE) takes the conflicting row lock, so it waits
// for the write, or the write waits for it and then finds no row.
const templateGrantForShareSQL = `SELECT 1 FROM template_group_admins WHERE group_id = $1 AND person = ANY($2) LIMIT 1 FOR SHARE`

const runTemplateCols = `t.id, t.scope, t.owner_id, t.group_id, t.kind, t.revision, t.created_at, t.created_by, t.updated_at, t.updated_by`

// runTemplateSelect reads the current revision of each matching template.
const runTemplateSelect = `SELECT ` + runTemplateCols + `, r.name, r.description, r.document
	FROM run_templates t JOIN run_template_revisions r ON r.template_id = t.id AND r.revision = t.revision`

// ListTemplatesVisible — see TemplateStore.
func (s PG) ListTemplatesVisible(ctx context.Context, person string, groups []string) ([]types.RunTemplate, error) {
	if groups == nil {
		groups = []string{}
	}
	const q = runTemplateSelect + `
		WHERE t.scope = 'org' OR (t.scope = 'person' AND t.owner_id = $1) OR (t.scope = 'group' AND t.group_id = ANY($2))
		ORDER BY lower(r.name), t.id`
	return collect(ctx, s.Pool, "list", "run templates", q, []any{person, groups}, scanRunTemplate)
}

// GetTemplate — see TemplateStore.
func (s PG) GetTemplate(ctx context.Context, id uuid.UUID) (types.RunTemplate, error) {
	return scanRunTemplate(s.Pool.QueryRow(ctx, runTemplateSelect+` WHERE t.id = $1`, id))
}

// GetTemplateRevision — see TemplateStore.
func (s PG) GetTemplateRevision(ctx context.Context, id uuid.UUID, revision int) (types.RunTemplate, error) {
	// The revision's own saver and time stand where the current revision's would.
	const q = `SELECT t.id, t.scope, t.owner_id, t.group_id, t.kind, t.revision, t.created_at, t.created_by,
		r.created_at, r.created_by, r.name, r.description, r.document
		FROM run_templates t JOIN run_template_revisions r ON r.template_id = t.id
		WHERE t.id = $1 AND r.revision = $2`
	t, err := scanRunTemplate(s.Pool.QueryRow(ctx, q, id, revision))
	if err == nil {
		t.Revision = revision
	}
	return t, err
}

// CreateTemplate — see TemplateStore.
func (s PG) CreateTemplate(ctx context.Context, t types.RunTemplate, grant *TemplateGrantCheck) (types.RunTemplate, error) {
	if t.ID == uuid.Nil || !t.Owner.Valid() || !t.Kind.Valid() {
		return types.RunTemplate{}, errors.New("store: create template: an id, a valid owner and a kind are required")
	}
	var out types.RunTemplate
	err := s.inTx(ctx, func(q Querier) error {
		if err := checkTemplateGrant(ctx, q, grant); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, templateAudienceLockSQL, templateAudienceKey(t.Owner)); err != nil {
			return err
		}
		var n int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM run_templates
			WHERE scope = $1 AND COALESCE(owner_id, '') = $2 AND COALESCE(group_id, '') = $3`,
			t.Owner.Scope, t.Owner.Person, t.Owner.Group).Scan(&n); err != nil {
			return err
		}
		if limit := TemplateLimit(t.Owner); n >= limit {
			return &TemplateLimitError{Limit: limit}
		}
		var at time.Time
		if err := q.QueryRow(ctx, `INSERT INTO run_templates
			(id, scope, owner_id, group_id, kind, name, description, revision, created_at, created_by, updated_at, updated_by)
			VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6, $7, 1, now(), $8, now(), $8)
			RETURNING created_at`, t.ID, t.Owner.Scope, t.Owner.Person, t.Owner.Group, t.Kind, t.Name, t.Description, t.CreatedBy).Scan(&at); err != nil {
			return err
		}
		out = t
		out.Revision, out.CreatedAt, out.UpdatedAt, out.UpdatedBy = 1, at, at, t.CreatedBy
		return insertTemplateRevision(ctx, q, out)
	})
	if err != nil {
		return types.RunTemplate{}, templateWriteErr("create", err)
	}
	return out, nil
}

// UpdateTemplate — see TemplateStore.
func (s PG) UpdateTemplate(ctx context.Context, t types.RunTemplate, expected int, grant *TemplateGrantCheck) (types.RunTemplate, error) {
	var out types.RunTemplate
	err := s.inTx(ctx, func(q Querier) error {
		if err := checkTemplateGrant(ctx, q, grant); err != nil {
			return err
		}
		const upd = `UPDATE run_templates AS t SET name = $2, description = $3, revision = revision + 1, updated_at = now(), updated_by = $4
			WHERE id = $1 AND revision = $5
			RETURNING ` + runTemplateCols + `, t.name, t.description, NULL::jsonb`
		var err error
		out, err = scanRunTemplate(q.QueryRow(ctx, upd, t.ID, t.Name, t.Description, t.UpdatedBy, expected))
		if errors.Is(err, ErrNotFound) {
			var current int
			if err := q.QueryRow(ctx, `SELECT revision FROM run_templates WHERE id = $1`, t.ID).Scan(&current); err != nil {
				return notFoundOr(err)
			}
			return &TemplateRevisionConflictError{Current: current}
		}
		if err != nil {
			return err
		}
		out.Document = t.Document
		return insertTemplateRevision(ctx, q, out)
	})
	if err != nil {
		return types.RunTemplate{}, templateWriteErr("update", err)
	}
	return out, nil
}

// DeleteTemplate — see TemplateStore.
func (s PG) DeleteTemplate(ctx context.Context, id uuid.UUID, grant *TemplateGrantCheck) (types.RunTemplate, error) {
	var out types.RunTemplate
	err := s.inTx(ctx, func(q Querier) error {
		if err := checkTemplateGrant(ctx, q, grant); err != nil {
			return err
		}
		var err error
		out, err = scanRunTemplate(q.QueryRow(ctx, `DELETE FROM run_templates AS t WHERE id = $1
			RETURNING `+runTemplateCols+`, t.name, t.description, NULL::jsonb`, id))
		return err
	})
	if err != nil {
		return types.RunTemplate{}, templateWriteErr("delete", err)
	}
	return out, nil
}

func checkTemplateGrant(ctx context.Context, q Querier, grant *TemplateGrantCheck) error {
	if grant == nil {
		return nil
	}
	var one int
	if err := q.QueryRow(ctx, templateGrantForShareSQL, grant.Group, grant.Persons).Scan(&one); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTemplateGrantGone
		}
		return err
	}
	return nil
}

func insertTemplateRevision(ctx context.Context, q Querier, t types.RunTemplate) error {
	_, err := q.Exec(ctx, `INSERT INTO run_template_revisions (template_id, revision, name, description, document, created_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, t.ID, t.Revision, t.Name, t.Description, []byte(t.Document), t.UpdatedAt, t.UpdatedBy)
	return err
}

func notFoundOr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// templateWriteErr keeps the store's own errors and names a unique-index
// violation as the name it is.
func templateWriteErr(verb string, err error) error {
	var pgErr *pgconn.PgError
	var limit *TemplateLimitError
	var conflict *TemplateRevisionConflictError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return ErrTemplateNameTaken
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrTemplateGrantGone), errors.As(err, &limit), errors.As(err, &conflict):
		return err
	}
	return fmt.Errorf("store: %s template: %w", verb, err)
}

func scanRunTemplate(row pgx.Row) (types.RunTemplate, error) {
	var t types.RunTemplate
	var owner, group *string
	var doc []byte
	err := row.Scan(&t.ID, &t.Owner.Scope, &owner, &group, &t.Kind, &t.Revision, &t.CreatedAt, &t.CreatedBy,
		&t.UpdatedAt, &t.UpdatedBy, &t.Name, &t.Description, &doc)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return types.RunTemplate{}, ErrNotFound
	case err != nil:
		return types.RunTemplate{}, err
	}
	if owner != nil {
		t.Owner.Person = *owner
	}
	if group != nil {
		t.Owner.Group = *group
	}
	if doc != nil {
		t.Document = json.RawMessage(doc)
	}
	return t, nil
}

// ListTemplateGroupAdmins — see TemplateStore.
func (s PG) ListTemplateGroupAdmins(ctx context.Context) ([]types.TemplateGroupAdmin, error) {
	const q = `SELECT a.group_id, a.person, a.granted_by, COALESCE(l.label, '')
		FROM template_group_admins a LEFT JOIN template_group_labels l ON l.group_id = a.group_id
		ORDER BY a.group_id, a.person`
	return collect(ctx, s.Pool, "list", "template group admins", q, nil, func(row pgx.Row) (types.TemplateGroupAdmin, error) {
		var g types.TemplateGroupAdmin
		err := row.Scan(&g.Group, &g.Person, &g.GrantedBy, &g.Label)
		return g, err
	})
}

// PutTemplateGroupAdmin — see TemplateStore.
func (s PG) PutTemplateGroupAdmin(ctx context.Context, g types.TemplateGroupAdmin) (types.TemplateGroupAdmin, error) {
	err := s.inTx(ctx, func(q Querier) error {
		if _, err := q.Exec(ctx, `INSERT INTO template_group_admins (group_id, person, granted_by, granted_at)
			VALUES ($1, $2, $3, now())
			ON CONFLICT (group_id, person) DO UPDATE SET granted_by = EXCLUDED.granted_by, granted_at = EXCLUDED.granted_at`,
			g.Group, g.Person, g.GrantedBy); err != nil {
			return err
		}
		if g.Label != "" {
			if _, err := q.Exec(ctx, `INSERT INTO template_group_labels (group_id, label) VALUES ($1, $2)
				ON CONFLICT (group_id) DO UPDATE SET label = EXCLUDED.label`, g.Group, g.Label); err != nil {
				return err
			}
		}
		return q.QueryRow(ctx, `SELECT COALESCE((SELECT label FROM template_group_labels WHERE group_id = $1), '')`, g.Group).Scan(&g.Label)
	})
	if err != nil {
		return types.TemplateGroupAdmin{}, fmt.Errorf("store: put template group admin: %w", err)
	}
	return g, nil
}

// DeleteTemplateGroupAdmin — see TemplateStore.
func (s PG) DeleteTemplateGroupAdmin(ctx context.Context, group, person string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM template_group_admins WHERE group_id = $1 AND person = $2`, group, person)
	if err != nil {
		return false, fmt.Errorf("store: delete template group admin: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// TemplateGroupAdminGroups — see TemplateStore.
func (s PG) TemplateGroupAdminGroups(ctx context.Context, persons []string) ([]string, error) {
	const q = `SELECT DISTINCT group_id FROM template_group_admins WHERE person = ANY($1) ORDER BY group_id`
	return collect(ctx, s.Pool, "list", "template group admin groups", q, []any{persons}, func(row pgx.Row) (string, error) {
		var g string
		err := row.Scan(&g)
		return g, err
	})
}

// TemplateGroupLabels — see TemplateStore.
func (s PG) TemplateGroupLabels(ctx context.Context, groups []string) (map[string]string, error) {
	out := map[string]string{}
	if len(groups) == 0 {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT group_id, label FROM template_group_labels WHERE group_id = ANY($1)`, groups)
	if err != nil {
		return nil, fmt.Errorf("store: read template group labels: %w", err)
	}
	var group, label string
	_, err = pgx.ForEachRow(rows, []any{&group, &label}, func() error { out[group] = label; return nil })
	if err != nil {
		return nil, fmt.Errorf("store: read template group labels: %w", err)
	}
	return out, nil
}

// EraseRunSetupsOf — see TemplateStore.
func (s PG) EraseRunSetupsOf(ctx context.Context, person string) (RunSetupsErased, error) {
	var out RunSetupsErased
	if person == "" {
		return out, errors.New("store: erase run setups: a person is required")
	}
	err := s.inTx(ctx, func(q Querier) error {
		count := func(dst *int, stmt string) error {
			tag, err := q.Exec(ctx, stmt, person)
			*dst += int(tag.RowsAffected())
			return err
		}
		for _, step := range []struct {
			dst  *int
			stmt string
		}{
			{&out.Templates, `DELETE FROM run_templates WHERE scope = 'person' AND owner_id = $1`},
			{&out.Grants, `DELETE FROM template_group_admins WHERE person = $1`},
			{&out.AuthorshipCleared, `UPDATE run_templates
				SET created_by = CASE WHEN created_by = $1 THEN '' ELSE created_by END,
				    updated_by = CASE WHEN updated_by = $1 THEN '' ELSE updated_by END
				WHERE created_by = $1 OR updated_by = $1`},
			{&out.AuthorshipCleared, `UPDATE run_template_revisions SET created_by = '' WHERE created_by = $1`},
			{&out.AuthorshipCleared, `UPDATE template_group_admins SET granted_by = '' WHERE granted_by = $1`},
		} {
			if err := count(step.dst, step.stmt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return RunSetupsErased{}, fmt.Errorf("store: erase run setups: %w", err)
	}
	return out, nil
}
