import { LogPre } from '@wardyn/ui';

export const RunLog = () => (
  <LogPre text={'$ wardyn run create --barrier fence\n✓ identity minted (spiffe://wardyn.local/run/8f2a)\n✓ barrier: Fence (oci/runc)\n→ agent started; egress gated by policy'} />
);
