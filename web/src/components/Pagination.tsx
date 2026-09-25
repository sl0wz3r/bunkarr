import { ChevronFirst, ChevronLast, ChevronLeft, ChevronRight } from 'lucide-react';
import { formatNumber } from '@/lib/format';
import { IconButton } from './Button';

/** Pagination shows "1–25 of 312" and first/previous/next/last buttons (page is 1-based). */
export function Pagination({ page, pageSize, total, onPage }: { page: number; pageSize: number; total: number; onPage: (page: number) => void }) {
  const pages = Math.max(1, Math.ceil(total / pageSize));
  const from = total === 0 ? 0 : (page - 1) * pageSize + 1;
  const to = Math.min(total, page * pageSize);
  return (
    <nav aria-label="Pagination" className="mt-3 flex flex-wrap items-center justify-between gap-2 text-sm text-ink-muted">
      <span>
        {formatNumber(from)}–{formatNumber(to)} of {formatNumber(total)}
      </span>
      <div className="flex items-center gap-1">
        <IconButton label="First page" icon={ChevronFirst} disabled={page <= 1} onClick={() => onPage(1)} />
        <IconButton label="Previous page" icon={ChevronLeft} disabled={page <= 1} onClick={() => onPage(page - 1)} />
        <span className="px-2">
          Page {formatNumber(page)} of {formatNumber(pages)}
        </span>
        <IconButton label="Next page" icon={ChevronRight} disabled={page >= pages} onClick={() => onPage(page + 1)} />
        <IconButton label="Last page" icon={ChevronLast} disabled={page >= pages} onClick={() => onPage(pages)} />
      </div>
    </nav>
  );
}
