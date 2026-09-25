import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { DataTable } from './DataTable';

describe('DataTable', () => {
  it('keeps visually hidden headers inside its scroll box', () => {
    // An sr-only header is position: absolute. Without a positioned scroll box it is placed
    // against the page, past the right edge of a wide table, and widens the whole page on phones.
    render(
      <DataTable
        caption="Rows"
        rows={[{ id: 1 }]}
        rowKey={(r) => r.id}
        columns={[
          { key: 'id', header: 'Id', cell: (r) => r.id },
          { key: 'actions', header: <span className="sr-only">Actions</span>, cell: () => null },
        ]}
      />,
    );
    const box = screen.getByRole('table', { name: 'Rows' }).parentElement!;
    expect(box).toHaveClass('overflow-x-auto');
    expect(box).toHaveClass('relative');
  });
});
