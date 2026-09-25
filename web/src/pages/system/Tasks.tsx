import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { CalendarClock, Eye, Pencil, Play, RefreshCw } from 'lucide-react';
import { useState, type ReactNode } from 'react';
import { Link, useNavigate } from 'react-router';
import { errorMessage } from '@/api/client';
import { listSchedules, runSchedule, updateSchedule } from '@/api/jobs';
import type { CronSchedule, Schedule } from '@/api/types';
import { Button, IconButton } from '@/components/Button';
import { CronInput } from '@/components/CronInput';
import { DataTable, type Column } from '@/components/DataTable';
import { Switch } from '@/components/Form';
import { Modal } from '@/components/Modal';
import { ErrorNotice, Notice } from '@/components/Notice';
import { EmptyState, Page } from '@/components/Page';
import { Badge } from '@/components/StatusBadge';
import { TASK_PRESETS, describeCron, validateCron } from '@/lib/cron';
import { formatDateTime, formatRelative } from '@/lib/format';
import { JOB_TYPE_LABELS } from '@/lib/labels';
import { jobTitle, keys, useNames, type Names } from '@/lib/lookups';

/** scheduleName is the server's description, or one built from the job type and params. */
function scheduleName(s: Schedule, names: Names): string {
  if (s.description) {
    return s.description;
  }
  return jobTitle({ type: s.jobType, params: s.params ?? {} }, names);
}

/** System → Tasks: every schedule with its next/last run; edit the cron, enable, run now or preview. */
export function Tasks() {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const names = useNames();
  const schedules = useQuery({ queryKey: keys.schedules, queryFn: listSchedules, refetchInterval: 30_000 });
  const [editing, setEditing] = useState<Schedule | null>(null);
  const [notice, setNotice] = useState<{ tone: 'success' | 'error'; body: ReactNode } | null>(null);
  // The schedule whose Run now or Preview is starting.
  const [running, setRunning] = useState<{ id: number; preview: boolean } | null>(null);

  const toggle = useMutation({
    mutationFn: (s: Schedule) => updateSchedule(s.id, { cron: s.cron, enabled: !s.enabled }),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.schedules }),
    onError: (e) => setNotice({ tone: 'error', body: errorMessage(e) }),
  });

  // runNow starts the schedule's job; a preview is a dry run (for retention: what would expire)
  // and opens the job, whose items show what a run would do.
  async function runNow(s: Schedule, preview: boolean) {
    setNotice(null);
    setRunning({ id: s.id, preview });
    try {
      const job = await runSchedule(s.id, preview ? { dryRun: true } : undefined);
      await qc.invalidateQueries({ queryKey: keys.jobs });
      if (preview) {
        navigate(`/activity/jobs/${job.id}`);
        return;
      }
      await qc.invalidateQueries({ queryKey: keys.schedules });
      setNotice({
        tone: 'success',
        body: (
          <>
            {scheduleName(s, names)} started.{' '}
            <Link className="text-accent hover:underline" to={`/activity/jobs/${job.id}`}>
              View job #{job.id}
            </Link>
          </>
        ),
      });
    } catch (e) {
      setNotice({ tone: 'error', body: `${scheduleName(s, names)}: ${errorMessage(e)}` });
    } finally {
      setRunning(null);
    }
  }

  const columns: Column<Schedule>[] = [
    {
      key: 'name',
      header: 'Task',
      cell: (s) => (
        <div>
          <div className="font-medium">{scheduleName(s, names)}</div>
          <div className="text-xs text-ink-muted">{JOB_TYPE_LABELS[s.jobType] ?? s.jobType}</div>
        </div>
      ),
    },
    {
      key: 'cron',
      header: 'Schedule',
      cell: (s) => (
        <div className="flex items-start gap-1">
          <div>
            <div>{describeCron(s.cron)}</div>
            <code className="text-xs text-ink-muted">{s.cron}</code>
          </div>
          <IconButton label={`Edit schedule of ${scheduleName(s, names)}`} icon={Pencil} onClick={() => setEditing(s)} />
        </div>
      ),
    },
    {
      key: 'next',
      header: 'Next run',
      className: 'whitespace-nowrap',
      cell: (s) =>
        !s.enabled ? (
          <span className="text-ink-muted">Disabled</span>
        ) : s.blockedReason ? (
          // The scheduler refuses this schedule's jobs (scheduleGate): no next run to promise.
          <div className="whitespace-normal">
            <Badge tone="warn">Not running</Badge>
            <div className="mt-1 text-xs text-ink-muted">{s.blockedReason}</div>
          </div>
        ) : (
          <span title={formatDateTime(s.nextRunAt)}>{formatRelative(s.nextRunAt)}</span>
        ),
    },
    {
      key: 'last',
      header: 'Last run',
      className: 'whitespace-nowrap',
      cell: (s) => <span title={formatDateTime(s.lastRunAt)}>{s.lastRunAt ? formatRelative(s.lastRunAt) : 'Never'}</span>,
    },
    {
      key: 'enabled',
      header: 'Enabled',
      cell: (s) => <Switch checked={s.enabled} label={`Enable ${scheduleName(s, names)}`} disabled={toggle.isPending} onChange={() => toggle.mutate(s)} />,
    },
    {
      key: 'run',
      header: <span className="sr-only">Actions</span>,
      className: 'whitespace-nowrap text-right',
      cell: (s) => (
        <div className="flex flex-wrap justify-end gap-1">
          <Button
            small
            variant="ghost"
            icon={Eye}
            busy={running?.id === s.id && running.preview}
            disabled={!!s.blockedReason}
            title={s.blockedReason ? `Cannot run: ${s.blockedReason}` : 'Dry run: shows what a run would do and changes nothing'}
            aria-label={`Preview ${scheduleName(s, names)}`}
            onClick={() => void runNow(s, true)}
          >
            Preview
          </Button>
          <Button
            small
            variant="ghost"
            icon={Play}
            busy={running?.id === s.id && !running.preview}
            disabled={!!s.blockedReason}
            title={s.blockedReason ? `Cannot run: ${s.blockedReason}` : undefined}
            aria-label={`Run ${scheduleName(s, names)} now`}
            onClick={() => void runNow(s, false)}
          >
            Run now
          </Button>
        </div>
      ),
    },
  ];

  const list = schedules.data;
  return (
    <Page
      title="Tasks"
      actions={
        <Button variant="ghost" icon={RefreshCw} onClick={() => void schedules.refetch()}>
          Refresh
        </Button>
      }
    >
      {notice && <Notice tone={notice.tone}>{notice.body}</Notice>}
      <ErrorNotice error={schedules.error} />
      <p className="mb-3 text-xs text-ink-muted">Schedules run in the container&apos;s time zone (TZ). Destination and Plex backup schedules can also be edited on their pages.</p>
      {list && list.length === 0 ? (
        <EmptyState icon={CalendarClock} title="No scheduled tasks">
          Destinations and Plex database backups add their schedules here.
        </EmptyState>
      ) : (
        <DataTable columns={columns} rows={list} rowKey={(s) => s.id} loading={schedules.isPending} caption="Scheduled tasks" />
      )}
      {editing && <EditSchedule schedule={editing} title={scheduleName(editing, names)} onClose={() => setEditing(null)} />}
    </Page>
  );
}

function EditSchedule({ schedule, title, onClose }: { schedule: Schedule; title: string; onClose: () => void }) {
  const qc = useQueryClient();
  const [value, setValue] = useState<CronSchedule>({ cron: schedule.cron, enabled: schedule.enabled });
  const error = validateCron(value.cron);
  const save = useMutation({
    mutationFn: () => updateSchedule(schedule.id, { cron: value.cron.trim(), enabled: value.enabled }),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: keys.schedules });
      onClose();
    },
  });
  return (
    <Modal
      title={`Schedule · ${title}`}
      size="lg"
      onClose={onClose}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" busy={save.isPending} disabled={!!error} onClick={() => save.mutate()}>
            Save
          </Button>
        </>
      }
    >
      <ErrorNotice error={save.error} />
      <CronInput label="Runs" value={value} onChange={setValue} presets={TASK_PRESETS} allowManual={false} />
      <div className="sm:ml-[12rem]">
        <div className="flex items-center gap-2 text-sm">
          <Switch checked={value.enabled} label="Enabled" onChange={(enabled) => setValue({ ...value, enabled })} />
          <span aria-hidden="true">{value.enabled ? 'Enabled' : 'Disabled (runs only with Run now)'}</span>
        </div>
      </div>
    </Modal>
  );
}
