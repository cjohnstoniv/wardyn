-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Console branding (#1215): a logo that `branding.logo_path` in the site config
-- delivered, rather than a person uploading it in the console. True while the
-- stored logo is the file's: the Branding card then offers no Remove logo (the
-- next apply would put the file back) and PUT /branding/settings refuses
-- remove_logo. An upload, a removal or the next apply without logo_path sets it
-- false again.
ALTER TABLE branding ADD COLUMN IF NOT EXISTS logo_from_file BOOLEAN NOT NULL DEFAULT false;
