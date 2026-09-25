import { Link } from 'react-router';
import { Page } from '@/components/Page';

export function NotFound() {
  return (
    <Page title="Not found">
      <p className="text-ink-muted">
        This page does not exist.{' '}
        <Link to="/" className="text-accent hover:underline">
          Back to Activity
        </Link>
      </p>
    </Page>
  );
}
