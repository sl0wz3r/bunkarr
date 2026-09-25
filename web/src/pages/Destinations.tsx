import { HardDrive } from 'lucide-react';
import { EmptyState, Page } from '@/components/Page';

export function Destinations() {
  return (
    <Page title="Destinations">
      <EmptyState icon={HardDrive} title="No destinations yet">
        Destinations are where backups go: a mounted NAS share, Backblaze B2, S3-compatible storage or SFTP.
      </EmptyState>
    </Page>
  );
}
