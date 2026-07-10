import { Table, TableHeader, TableBody, TableRow, TableHead, TableCell, TableCaption } from '@wardyn/ui';

const RunsTable = () => (
  <Table>
    <TableCaption>Recent runs on this host</TableCaption>
    <TableHeader>
      <TableRow>
        <TableHead>Run</TableHead>
        <TableHead>Barrier</TableHead>
        <TableHead>Status</TableHead>
      </TableRow>
    </TableHeader>
    <TableBody>
      <TableRow><TableCell>build-site</TableCell><TableCell>Fence</TableCell><TableCell>completed</TableCell></TableRow>
      <TableRow><TableCell>scan-repo</TableCell><TableCell>Wall</TableCell><TableCell>running</TableCell></TableRow>
    </TableBody>
  </Table>
);
export const InRunsTable = RunsTable;
