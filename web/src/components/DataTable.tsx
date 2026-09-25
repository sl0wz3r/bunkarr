import type { ReactNode } from 'react';

/** Column describes one table column: its header and how to render a row's cell. */
export interface Column<T> {
  key: string;
  header: ReactNode;
  cell: (row: T) => ReactNode;
  className?: string;
}

/**
 * DataTable renders rows in the *arr table style. It scrolls horizontally on narrow screens and
 * shows `empty` (or "Loading…") in place of rows. The scroll box is `relative` so visually hidden
 * (sr-only, position: absolute) headers stay inside it instead of widening the page.
 */
export function DataTable<T>({
  columns,
  rows,
  rowKey,
  empty = 'Nothing to show.',
  loading,
  caption,
  rowClassName,
}: {
  columns: Column<T>[];
  rows: T[] | undefined;
  rowKey: (row: T) => string | number;
  empty?: ReactNode;
  loading?: boolean;
  caption?: string;
  rowClassName?: (row: T) => string;
}) {
  const list = rows ?? [];
  return (
    <div className="relative overflow-x-auto rounded border border-line bg-panel">
      <table className="w-full border-collapse text-sm">
        {caption && <caption className="sr-only">{caption}</caption>}
        <thead>
          <tr className="border-b border-line text-left text-ink-muted">
            {columns.map((c) => (
              <th key={c.key} scope="col" className={`px-3 py-2 font-medium ${c.className ?? ''}`}>
                {c.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {list.length === 0 ? (
            <tr>
              <td colSpan={columns.length} className="px-3 py-6 text-center text-ink-muted">
                {loading ? 'Loading…' : empty}
              </td>
            </tr>
          ) : (
            list.map((row) => (
              <tr key={rowKey(row)} className={`border-b border-line/60 last:border-b-0 hover:bg-panel-2/50 ${rowClassName?.(row) ?? ''}`}>
                {columns.map((c) => (
                  <td key={c.key} className={`px-3 py-2 align-top ${c.className ?? ''}`}>
                    {c.cell(row)}
                  </td>
                ))}
              </tr>
            ))
          )}
        </tbody>
      </table>
    </div>
  );
}
