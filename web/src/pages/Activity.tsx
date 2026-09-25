import { History as HistoryIcon, ListOrdered } from 'lucide-react';
import { EmptyState, Page } from '@/components/Page';

export function Queue() {
  return (
    <Page title="Queue">
      <EmptyState icon={ListOrdered} title="Nothing running">
        Running and queued backup jobs will appear here with their progress and throughput.
      </EmptyState>
    </Page>
  );
}

export function History() {
  return (
    <Page title="History">
      <EmptyState icon={HistoryIcon} title="No history yet">
        Finished syncs, database backups and restores will be listed here with their logs.
      </EmptyState>
    </Page>
  );
}
