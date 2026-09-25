import { Database } from 'lucide-react';
import { EmptyState, Page } from '@/components/Page';

export function Library() {
  return (
    <Page title="Library">
      <EmptyState icon={Database} title="No sources yet">
        Connect Plex to import your library sections as backup sources. Each item will show whether it gets a full
        backup or a manifest entry, and why.
      </EmptyState>
    </Page>
  );
}
