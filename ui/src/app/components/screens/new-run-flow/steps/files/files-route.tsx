/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Files step as the route mounts it: the public step with the person-created
// drives arm in its seam.

import { FilesStep } from "./files-step";
import { CreateUserDrive } from "./create-user-drive";
import type { NewRunScreenProps } from "../../new-run-route";

export const FilesRoute = (props: NewRunScreenProps) => <FilesStep {...props} createUserDrive={(ctx) => <CreateUserDrive {...ctx} />} />;
