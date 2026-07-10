import { DropdownMenuLabel } from '@wardyn/ui';

export const SectionLabel = () => (
  <div style={{ width: 220, border: '1px solid var(--border)', borderRadius: 8, padding: 4, background: 'var(--popover, var(--card))' }}>
    <DropdownMenuLabel>Run actions</DropdownMenuLabel>
    <div style={{ padding: '6px 8px', fontSize: 14 }}>Approve</div>
    <div style={{ padding: '6px 8px', fontSize: 14 }}>Deny with review</div>
  </div>
);
