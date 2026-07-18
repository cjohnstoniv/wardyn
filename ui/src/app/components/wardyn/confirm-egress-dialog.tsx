/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Check } from "lucide-react";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../ui/alert-dialog";
import { Mono } from "./code-block";

// Shared untrusted-content confirm for approving egress hosts. Every
// entry point that promotes a suggested/observed/detected host into a
// workspace's approved_egress — the Workspaces screen and the import
// Scan/Configure/Verify panes — must ask this same question before the PUT,
// since the host name itself came from untrusted content (the workspace's own
// files, or a run's denied egress) and, once approved, is reachable by every
// future run that mounts the workspace.
export function ConfirmEgressDialog({
  hosts,
  onOpenChange,
  onConfirm,
}: {
  // null/empty = closed. One host = singular copy; more = bulk copy + list.
  hosts: string[] | null;
  onOpenChange: (open: boolean) => void;
  onConfirm: () => void;
}) {
  const open = !!hosts && hosts.length > 0;
  const bulk = (hosts?.length ?? 0) > 1;
  const single = hosts?.[0];

  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>
            {bulk ? `Approve egress to ${hosts!.length} hosts?` : `Approve egress to ${single}?`}
          </AlertDialogTitle>
          <AlertDialogDescription>
            {bulk ? "These hosts aren't" : "This host isn't"} approved for this workspace yet — they
            came from untrusted content (the workspace&apos;s own files, or a run&apos;s denied egress).
            Approving allows every future run that mounts this workspace to reach {bulk ? "them" : "it"}.
          </AlertDialogDescription>
        </AlertDialogHeader>
        {bulk && (
          <ul className="max-h-40 space-y-1 overflow-y-auto rounded-md border border-border bg-muted/40 px-3 py-2">
            {hosts!.map((h) => (
              <li key={h}>
                <Mono className="text-foreground">{h}</Mono>
              </li>
            ))}
          </ul>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            onClick={(e) => {
              e.preventDefault();
              onConfirm();
            }}
          >
            <Check className="size-4" /> {bulk ? "Approve hosts" : "Approve host"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
