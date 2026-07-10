import { StatusChip } from '@wardyn/ui';

export const Statuses = () => (
  <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
    <StatusChip status="ready" />
    <StatusChip status="needs-setup" />
    <StatusChip status="connected" />
    <StatusChip status="incompatible" />
    <StatusChip status="checking" />
  </div>
);
