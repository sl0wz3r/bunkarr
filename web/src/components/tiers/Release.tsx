import { useQueryClient } from '@tanstack/react-query';
import { RefreshCw, Unlock } from 'lucide-react';
import { useState } from 'react';
import { useNavigate } from 'react-router';
import { applyRelease, isReleasePreview, startReleasePreview, tierRevisionOf } from '@/api/tiers';
import type { Job } from '@/api/types';
import { Button } from '@/components/Button';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { ErrorNotice, Notice } from '@/components/Notice';
import { formatBytes, formatNumber } from '@/lib/format';
import { isActive, keys } from '@/lib/lookups';

/**
 * ReleasePreviewDialog confirms the first step of a release (S15): a dry-run sync with
 * releaseDemoted that lists the kept files a release would free at the destination. Nothing is
 * changed; the job page it opens is where "Apply release" confirms the real run.
 */
export function ReleasePreviewDialog({
  destinationId,
  destinationName,
  files,
  bytes,
  onClose,
}: {
  destinationId: number;
  destinationName: string;
  files: number;
  bytes: number;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  return (
    <ConfirmDialog
      title="Release kept files"
      confirmLabel="Preview the release"
      onClose={onClose}
      onConfirm={async () => {
        const job = await startReleasePreview(destinationId);
        await qc.invalidateQueries({ queryKey: keys.jobs });
        navigate(`/activity/jobs/${job.id}`);
      }}
    >
      <p>
        <strong>{destinationName}</strong> keeps {formatNumber(files)} {files === 1 ? 'file' : 'files'} ({formatBytes(bytes)}) that the rules no longer make full.
        A tier change never removes a backup on its own.
      </p>
      <p>
        This first runs a preview (a dry run): nothing is changed. It lists the files a release would move into the destination's retention folder, where
        they are deleted after the retention period. Review it, then confirm with <strong>Apply release</strong> on its job page.
      </p>
    </ConfirmDialog>
  );
}

/**
 * ReleaseJobNotice is the release part of a sync's job page. For a finished release preview it
 * offers "Apply release", which starts the real run with releaseDemoted, allowChanges, releaseOf
 * (this preview) and releaseRevision (the rule revision it evaluated); the server refuses it (409)
 * when the rules changed since. For a real release it says what it applies.
 */
export function ReleaseJobNotice({ job }: { job: Job }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [confirm, setConfirm] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const preview = isReleasePreview(job);
  const finished = job.status === 'completed' || job.status === 'completed_with_warnings';
  if (job.type !== 'sync' || !job.params?.releaseDemoted) {
    return null;
  }
  const destinationId = job.params.destinationId;
  // The sync's stats count the release items (a dry run: what it would release).
  const released = typeof job.stats?.filesReleased === 'number' ? job.stats.filesReleased : null;
  const bytes = typeof job.stats?.bytesReleased === 'number' ? job.stats.bytesReleased : 0;

  if (!preview) {
    return (
      <Notice tone="info" title="Release">
        <p>
          This sync releases the kept files of release preview #{job.params.releaseOf} (rule revision {job.params.releaseRevision}): they move into retention
          and are deleted after the retention period. A file whose tier is full again, or any file once the rules changed, is left alone.
          {released !== null && ` Released: ${formatNumber(released)} ${released === 1 ? 'file' : 'files'} (${formatBytes(bytes)}).`}
        </p>
      </Notice>
    );
  }

  const revision = tierRevisionOf(job);

  async function apply() {
    const next = await applyRelease(job);
    await qc.invalidateQueries({ queryKey: keys.jobs });
    navigate(`/activity/jobs/${next.id}`);
  }

  async function again() {
    if (!destinationId) return;
    setError(null);
    try {
      const next = await startReleasePreview(destinationId);
      await qc.invalidateQueries({ queryKey: keys.jobs });
      navigate(`/activity/jobs/${next.id}`);
    } catch (e) {
      setError(e);
    }
  }

  return (
    <Notice tone="info" title="Release preview">
      <ErrorNotice error={error} />
      <p>
        {released !== null
          ? `${formatNumber(released)} kept ${released === 1 ? 'file' : 'files'} (${formatBytes(bytes)}) would be released: `
          : 'The kept files a release would free are listed below as retain items: '}
        they are no longer full at this destination and would move into retention. Nothing was changed.
        {revision > 0 && ` Evaluated at rule revision ${revision}.`}
      </p>
      {isActive(job) ? (
        <p className="mt-1 text-ink-muted">Apply release is offered when the preview has finished.</p>
      ) : !finished ? (
        <p className="mt-1">This preview did not finish, so it cannot be applied. Run it again.</p>
      ) : revision === 0 ? (
        <p className="mt-1">This preview recorded no rule revision, so it cannot be applied. Run it again.</p>
      ) : null}
      <div className="mt-2 flex flex-wrap gap-2">
        {finished && revision > 0 && (
          <Button small variant="primary" icon={Unlock} onClick={() => setConfirm(true)}>
            Apply release
          </Button>
        )}
        {!isActive(job) && (
          <Button small icon={RefreshCw} onClick={() => void again()}>
            Run the release preview again
          </Button>
        )}
      </div>
      {confirm && (
        <ConfirmDialog title="Apply release" confirmLabel="Start release" danger onClose={() => setConfirm(false)} onConfirm={apply}>
          <p>
            Release {released !== null ? `${formatNumber(released)} ${released === 1 ? 'file' : 'files'} (${formatBytes(bytes)})` : 'the files of this preview'}: they move
            into the destination's retention folder and are deleted after the retention period.
          </p>
          <p>
            Only the files this preview listed are released, and only while the rules are still at revision {revision}. If they changed, nothing is released
            and you run the preview again. A file whose tier is full again is left alone. The sync also runs any other changes the mass-change guard would hold.
          </p>
        </ConfirmDialog>
      )}
    </Notice>
  );
}
