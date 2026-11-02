-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The metadata.uid of the custom resource a k8s workload was last reported from ('' for an API
-- workload, and for a row recorded before this column). A status goes back naming it, and the runner
-- writes the status only to the resource that still carries it, so an answer meant for a deleted
-- resource never lands on one recreated under the same name.
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS k8s_uid TEXT NOT NULL DEFAULT '';
