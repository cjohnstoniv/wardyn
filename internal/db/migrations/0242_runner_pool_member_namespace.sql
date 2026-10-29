-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The Kubernetes namespace an organization runner serves a pool in. One org runner can place runs in
-- several namespaces; the namespace belongs to the membership ("runner R serves pool P in namespace N"),
-- so one pool can hold runners of two clusters whose team namespaces are named differently. '' is the
-- runner's default (first) namespace, which is every existing row and every other kind of member. The
-- one-pool rule for an org runner is decided per namespace by the writer, under the runner's row lock.
ALTER TABLE runner_pool_members
    ADD COLUMN IF NOT EXISTS runs_namespace TEXT NOT NULL DEFAULT ''
        CHECK (runs_namespace = '' OR runs_namespace ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$');
