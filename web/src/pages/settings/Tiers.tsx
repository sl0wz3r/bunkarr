import { useQuery, useQueryClient } from '@tanstack/react-query';
import { ChevronDown, ChevronRight, Eye, Layers, Plus, RefreshCw, RotateCcw, Save } from 'lucide-react';
import { useEffect, useState } from 'react';
import { ApiError } from '@/api/client';
import {
  getTierFields,
  getTierPresets,
  getTierRules,
  listFlags,
  previewTiers,
  saveTierRules,
  type TierDestinationPreview,
  type TierPreset,
  type TierPreview,
  type TierRule,
  type TierRuleInput,
  type TierRuleSet,
} from '@/api/tiers';
import { Button } from '@/components/Button';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { inputClass } from '@/components/Form';
import { ErrorNotice, Notice } from '@/components/Notice';
import { Page } from '@/components/Page';
import { Badge } from '@/components/StatusBadge';
import { DeletedIntegrations } from '@/components/tiers/DeletedIntegrations';
import { FlagsTable, flagsKey } from '@/components/tiers/Flags';
import { PreviewPanel } from '@/components/tiers/PreviewPanel';
import { ReleasePreviewDialog } from '@/components/tiers/Release';
import { RuleCard } from '@/components/tiers/RuleCard';
import { TierBadge } from '@/components/tiers/TierBadge';
import {
  draftOf,
  draftProblems,
  emptyRule,
  fieldMap,
  placeWarnings,
  sameRules,
  toInputs,
  type DraftRule,
  type WarningsByKey,
} from '@/components/tiers/tierText';
import { formatBytes, formatNumber } from '@/lib/format';
import { useDestinations, useSources } from '@/lib/lookups';

export const tierRulesKey = ['tiers', 'rules'] as const;

/** Base is the saved rule set the draft started from: its revision goes with the next save. */
interface Base {
  revision: number;
  rules: TierRule[];
  /** The saved rules as the editor sends them, to tell whether the draft changed. */
  inputs: TierRuleInput[];
}

/**
 * rekey gives rules just loaded from a save the keys of the draft that was saved (same order and
 * conditions), so warnings placed on those keys stay next to their conditions.
 */
function rekey(saved: DraftRule[], old: DraftRule[]): DraftRule[] {
  return saved.map((r, i) => {
    const o = old[i];
    if (!o) return r;
    return { ...r, key: o.key, conditions: r.conditions.map((c, j) => ({ ...c, key: o.conditions[j]?.key ?? c.key })) };
  });
}

/**
 * Settings → Tiers (/settings/tiers, phase2-3.md §8, §16): the ordered rule set that decides per
 * file and destination whether a file is copied (full), only listed in manifests, or skipped.
 *
 * D1 (the user's decision): with no rules every file is full, so an upgraded install behaves as
 * before. Presets are only loaded into the editor; nothing applies until Save, which sends the
 * revision the editor was loaded at (a 409 offers a reload). Preview shows what a draft would do
 * without writing anything. Files that stop being full are kept (S15) until a release, offered
 * for the saved rules' kept files.
 */
export function Tiers() {
  const qc = useQueryClient();
  const rules = useQuery({ queryKey: tierRulesKey, queryFn: getTierRules });
  const fields = useQuery({ queryKey: ['tiers', 'fields'], queryFn: getTierFields });
  const presets = useQuery({ queryKey: ['tiers', 'presets'], queryFn: getTierPresets });
  const flags = useQuery({ queryKey: flagsKey, queryFn: listFlags });
  const destinations = useDestinations();
  const sources = useSources();

  const [base, setBase] = useState<Base | null>(null);
  const [draft, setDraft] = useState<DraftRule[]>([]);
  const [warnings, setWarnings] = useState<WarningsByKey>(new Map());
  const [loadedPreset, setLoadedPreset] = useState<TierPreset | null>(null);
  const [presetId, setPresetId] = useState('');
  const [confirmPreset, setConfirmPreset] = useState<TierPreset | null>(null);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);
  const [conflict, setConflict] = useState(false);
  const [saved, setSaved] = useState<{ revision: number; warnings: number } | null>(null);
  const [preview, setPreview] = useState<{ result: TierPreview; inputs: TierRuleInput[] } | null>(null);
  const [previewing, setPreviewing] = useState(false);
  const [previewError, setPreviewError] = useState<unknown>(null);
  const [release, setRelease] = useState<TierDestinationPreview | null>(null);
  const [showFlags, setShowFlags] = useState(false);

  const fmap = fieldMap(fields.data);
  const inputs = toInputs(draft, fmap);
  const dirty = !!base && !sameRules(inputs, base.inputs);
  const problems = draftProblems(draft, fmap);

  function load(rs: TierRuleSet) {
    const d = rs.rules.map((r) => draftOf(r));
    setBase({ revision: rs.revision, rules: rs.rules, inputs: toInputs(d, new Map()) });
    setDraft(d);
    setWarnings(new Map());
    setLoadedPreset(null);
    setConflict(false);
    setSaveError(null);
    setSaved(null);
  }

  // The editor starts from the saved rules once they arrive.
  useEffect(() => {
    if (rules.data && !base) load(rules.data);
  }, [rules.data, base]);

  async function reload() {
    const r = await rules.refetch();
    if (r.data) load(r.data);
  }

  function discard() {
    if (!base) return;
    setDraft(base.rules.map((r) => draftOf(r)));
    setWarnings(new Map());
    setLoadedPreset(null);
  }

  function applyPreset(p: TierPreset) {
    setDraft(p.rules.map((r) => draftOf(r, false)));
    setWarnings(new Map());
    setLoadedPreset(p);
    setSaved(null);
  }

  function move(index: number, delta: -1 | 1) {
    const to = index + delta;
    if (to < 0 || to >= draft.length) return;
    const next = [...draft];
    [next[index], next[to]] = [next[to], next[index]];
    setDraft(next);
  }

  async function runPreview(body: TierRuleInput[] | null, compareTo: TierRuleInput[]) {
    setPreviewing(true);
    setPreviewError(null);
    try {
      const result = await previewTiers(body === null ? {} : { rules: body });
      setPreview({ result, inputs: compareTo });
    } catch (e) {
      setPreviewError(e);
    } finally {
      setPreviewing(false);
    }
  }

  async function save() {
    if (!base) return;
    setSaving(true);
    setSaveError(null);
    setConflict(false);
    try {
      const res = await saveTierRules(base.revision, inputs);
      const next = rekey(
        res.rules.map((r) => draftOf(r)),
        draft,
      );
      const savedInputs = toInputs(next, fmap);
      qc.setQueryData<TierRuleSet>(tierRulesKey, { revision: res.revision, rules: res.rules });
      setBase({ revision: res.revision, rules: res.rules, inputs: savedInputs });
      setDraft(next);
      setWarnings(placeWarnings(res.warnings, draft));
      setLoadedPreset(null);
      setSaved({ revision: res.revision, warnings: res.warnings?.length ?? 0 });
      // What the saved rules leave kept (S15): the preview of the saved rules offers the release.
      void runPreview(null, savedInputs);
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        setConflict(true);
      } else {
        setSaveError(e);
      }
    } finally {
      setSaving(false);
    }
  }

  const ready = !!base && fields.isSuccess && destinations.isSuccess;
  const loadError = rules.error ?? fields.error ?? destinations.error;
  const sourceName = (id: number) => sources.data?.find((s) => s.id === id)?.name ?? `source #${id}`;
  const flagCount = flags.data?.length ?? 0;
  const savedPreview = preview && typeof preview.result.revision === 'number' && base && preview.result.revision === base.revision ? preview.result : null;
  const keptAt = (savedPreview?.destinations ?? []).filter((d) => d.kept.files > 0);
  const outdated = !!preview && !sameRules(preview.inputs, inputs);

  // A confirmed removal changes decisions: the preview on screen is computed again, as it was
  // asked for (the saved rules, or the draft it previewed).
  async function afterRemovalConfirmed() {
    if (!preview) return;
    const saved = typeof preview.result.revision === 'number';
    await runPreview(saved ? null : preview.inputs, preview.inputs);
  }

  return (
    <Page
      title={
        <span className="flex items-center gap-2">
          Tiers {dirty && <Badge tone="warn">Unsaved changes</Badge>}
        </span>
      }
      actions={
        <>
          <Button variant="ghost" icon={Eye} busy={previewing} disabled={!ready || problems.length > 0} onClick={() => void runPreview(dirty ? inputs : null, inputs)}>
            Preview
          </Button>
          <Button variant="ghost" icon={RotateCcw} disabled={!dirty} onClick={discard}>
            Discard changes
          </Button>
          <Button variant="primary" icon={Save} busy={saving} disabled={!ready || !dirty || problems.length > 0} onClick={() => void save()}>
            Save
          </Button>
        </>
      }
    >
      <ErrorNotice error={loadError} />
      {conflict && (
        <Notice tone="error" title="The rules were changed elsewhere">
          <p>
            Someone saved the tier rules after this page loaded them, so this save was refused. Reload them to see the current rules; your unsaved changes here
            are discarded.
          </p>
          <Button small className="mt-2" icon={RefreshCw} onClick={() => void reload()}>
            Reload rules
          </Button>
        </Notice>
      )}
      <ErrorNotice error={saveError} />
      {saved && !dirty && (
        <Notice tone="success">
          Saved (revision {saved.revision}). {saved.warnings > 0 && `${saved.warnings} ${saved.warnings === 1 ? 'value is' : 'values are'} not known to any fresh index: see the warnings below.`}{' '}
          The next sync of each destination uses these rules.
        </Notice>
      )}
      {keptAt.map((d) => (
        <Notice key={d.destinationId} tone="info" title={`${d.name} keeps files the rules no longer make full`}>
          <p>A tier change never removes a backup: these stay at the destination until you release them.</p>
          <Button small className="mt-2" onClick={() => setRelease(d)}>
            Release {formatNumber(d.kept.files)} {d.kept.files === 1 ? 'file' : 'files'} ({formatBytes(d.kept.bytes)}) at {d.name}…
          </Button>
        </Notice>
      ))}
      <DeletedIntegrations onConfirmed={afterRemovalConfirmed} />
      {loadedPreset && (
        <Notice tone="info" title={`Preset loaded into the editor: ${loadedPreset.name}`}>
          <p>{loadedPreset.description}</p>
          <p className="mt-1 font-medium">Nothing has changed yet. Preview it, then Save to apply it.</p>
        </Notice>
      )}

      {!ready ? (
        !loadError && <p className="text-ink-muted">Loading…</p>
      ) : (
        <>
          <p className="mb-4 max-w-3xl text-sm text-ink-muted">
            Rules decide, for each file and destination, whether it is copied (Full), only listed in the destination's manifests (Manifest only), or left
            out (Skip). The first rule that matches decides. When a fact a rule needs is unknown (a stale cache), a more protective rule that might match
            decides instead: unknown never lowers protection. A file that stops being full is kept at the destination until you release it.
          </p>

          <div className="mb-4 flex flex-wrap items-end gap-2">
            <label className="text-sm">
              <span className="mb-1 block text-ink-muted">Preset</span>
              <select aria-label="Preset" className={`${inputClass} w-72`} value={presetId} onChange={(e) => setPresetId(e.target.value)}>
                <option value="">Choose a preset…</option>
                {(presets.data ?? []).map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name}
                  </option>
                ))}
              </select>
            </label>
            <Button
              icon={Layers}
              disabled={!presetId}
              onClick={() => {
                const p = presets.data?.find((x) => x.id === presetId);
                if (!p) return;
                if (draft.length > 0 || dirty) setConfirmPreset(p);
                else applyPreset(p);
              }}
            >
              Load into editor
            </Button>
          </div>
          <ErrorNotice error={presets.error} />

          <ol className="mb-4 space-y-3" aria-label="Tier rules">
            <li className="rounded border border-line/60 bg-panel-2/40 p-3 text-sm">
              <div className="flex flex-wrap items-center gap-2">
                <span className="font-medium">Irreplaceable</span>
                <span className="text-ink-muted">→</span>
                <TierBadge tier="full" />
                <Badge>Built-in</Badge>
                <span className="text-xs text-ink-muted">A file flagged irreplaceable is full at every destination; no rule can change that.</span>
                <Button small variant="ghost" icon={showFlags ? ChevronDown : ChevronRight} className="ml-auto" aria-expanded={showFlags} onClick={() => setShowFlags(!showFlags)}>
                  {flags.isSuccess ? `${formatNumber(flagCount)} ${flagCount === 1 ? 'flag' : 'flags'}` : 'Flags'}
                </Button>
              </div>
              {showFlags && (
                <div className="mt-3">
                  <ErrorNotice error={flags.error} />
                  <FlagsTable flags={flags.data} sourceName={sourceName} loading={flags.isPending} />
                </div>
              )}
            </li>
            {draft.map((r, i) => (
              <RuleCard
                key={r.key}
                rule={r}
                index={i}
                count={draft.length}
                fields={fields.data ?? []}
                destinations={destinations.data ?? []}
                problems={problems.filter((p) => p.ruleKey === r.key)}
                warnings={warnings}
                onChange={(next) => setDraft(draft.map((x) => (x.key === r.key ? next : x)))}
                onMove={(delta) => move(i, delta)}
                onRemove={() => setDraft(draft.filter((x) => x.key !== r.key))}
              />
            ))}
            <li className="rounded border border-line/60 bg-panel-2/40 p-3 text-sm">
              <div className="flex flex-wrap items-center gap-2">
                <span className="font-medium">Everything else</span>
                <span className="text-ink-muted">→</span>
                <TierBadge tier="full" />
                <Badge>Built-in</Badge>
                <span className="text-xs text-ink-muted">A file no rule matches is copied in full.</span>
              </div>
            </li>
          </ol>
          <div className="mb-4 flex flex-wrap items-center gap-3">
            <Button icon={Plus} disabled={draft.length >= 100} onClick={() => setDraft([...draft, emptyRule(draft.length + 1)])}>
              Add rule
            </Button>
            {draft.length === 0 && <span className="text-sm text-ink-muted">No rules: every file is copied in full to every destination (the default).</span>}
          </div>
          {problems.length > 0 && (
            <p className="mb-4 text-sm text-danger">
              {problems.length === 1 ? '1 problem' : `${problems.length} problems`} to fix before previewing or saving (shown on the rules above).
            </p>
          )}
          <Notice tone="info">Plex DB and *arr config backups are always full: they are not tiered.</Notice>

          <ErrorNotice error={previewError} />
          {preview && (
            <PreviewPanel
              preview={preview.result}
              fields={fmap}
              outdated={outdated}
              onRerun={() => void runPreview(dirty ? inputs : null, inputs)}
              onRemovalConfirmed={afterRemovalConfirmed}
            />
          )}
        </>
      )}

      {confirmPreset && (
        <ConfirmDialog
          title="Load preset"
          confirmLabel="Load into editor"
          onClose={() => setConfirmPreset(null)}
          onConfirm={() => applyPreset(confirmPreset)}
        >
          <p>
            Replace the rules in the editor with <strong>{confirmPreset.name}</strong>? {dirty && 'Your unsaved changes are lost. '}Nothing is saved or applied
            until you click Save.
          </p>
        </ConfirmDialog>
      )}
      {release && (
        <ReleasePreviewDialog
          destinationId={release.destinationId}
          destinationName={release.name}
          files={release.kept.files}
          bytes={release.kept.bytes}
          onClose={() => setRelease(null)}
        />
      )}
    </Page>
  );
}
