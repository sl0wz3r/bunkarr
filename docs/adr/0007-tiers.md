# 0007. Backup tiers: full by default, unknown is protective, demotion keeps copies, Maintainerr by version

- Status: accepted
- Date: 2026-09-26
- Design: [`docs/design/phase2-3.md`](../design/phase2-3.md) revision 4 (D1, D5, D6, D12, D15;
  S10, S14, S15; §6.2, §8, §21)

## Context

Phase 3 lets each destination hold a file in one of three tiers: `full` (copied), `manifest`
(not copied, listed in the destination's manifests so it can be acquired again) or `skip`. Rules
decide the tier from facts in Bunkarr's caches: the *arrs, the Plex library index, Tautulli plays,
Seerr requests and Maintainerr's pending deletions. A tier that is too low loses media that cannot
be downloaded again, so every choice below prefers a copy too many over a copy too few. The
facts about Tautulli 2.18.1, Seerr 3.4.1 and Maintainerr 3.4.1 and 3.29.0 come from the Phase 3
fixture spike against real apps (`testdata/tautulli`, `testdata/seerr`, `testdata/maintainerr`).

## Decision 1: with no rules, every file is full (D1)

The spec's default rule set copies only items tagged `bunkarr-full` and lists everything else as
`manifest`. The user decided against it: media stays backed up in full by default.

- With no rules every file is `full`. A sync then loads no facts at all, and its plan is the same
  as Phase 1's, item for item (acceptance 8; `TestUpgradeFromPhase2` runs the real Phase 2 binary
  and this one on copies of the same database).
- The first row, "Irreplaceable → full", and the last row, "Everything else → full", are built in
  and cannot be removed.
- The spec's set is the preset "Manifest by default". Presets are loaded into the editor only;
  nothing changes until the user saves, and the preview shows the effect first.

**Rejected:** the spec's default (an upgrade would stop copying most of a library without the
user doing anything); a first-run wizard that picks a rule set (the same risk, one click away).

## Decision 2: an unknown fact never lowers protection (S14)

Every condition is true, false or **unknown**. A fact is unknown when its integration is not set
up, its cache is not fresh (D15), the file is not known to it, the evidence conflicts (two *arrs
claim the file, several Plex items with different play histories, a file in several Plex
libraries: D12), or the value is only a lower bound (Tautulli with history off for a section).
As built, these are unknown too: files under an *arr root folder that has no path mapping, files
of a deleted *arr integration that no live *arr claims, and Tautulli, Seerr or Maintainerr rows
recorded against another Plex server than the linked index.

- Unknown is never true, and `not unknown` is unknown.
- A rule that is unknown and more protective than the rule that matches later wins over it
  (`full` > `manifest` > `skip`). The fallback is `full`. So a stale cache, a broken integration
  or a deleted one can only make files more protected.
- A copy made only because a fact is unknown (`unknownPromoted`) counts as a change for the
  mass-change guard, and when free space runs short it is held rather than failing the job. A
  briefly stale cache therefore cannot flood a destination.
- The preview and the Library item view show which facts were unknown and why.

**Consequence:** a Maintainerr outage longer than `staleAfterHours` copies what a "pending → skip"
rule would have skipped (acceptance 7). A deleted *arr's files stay full until the user confirms
the removal; the confirm action is not built yet (DEFERRED.md).

## Decision 3: a demotion keeps the copy until a confirmed release (S15, D6)

When a file stops being `full` at a destination, what the destination holds is **kept**: not
updated, not repaired, not retained while the source file exists. A move of backed-up content
still follows the content, whatever its new tier.

A kept file leaves the destination only when its source name disappears (retained as in Phase 1)
or by a **release**:
1. A release dry run lists every kept file and records the rule revision.
2. "Apply release" runs a sync that carries that dry run (`releaseOf`) and the revision
   (`releaseRevision`). It releases only files that were in that dry run and are still not
   `full`. If the rules changed since the preview, nothing is released.
3. A released file goes into retention with reason `released` and expires after `deletedDays`,
   like a deleted file. The mass-change guard counts releases as changes. Retention never expires
   a file flagged irreplaceable.

**Rejected:** deleting on demotion (one mis-edited rule, or a fact that changes between preview
and confirmation, could empty a destination); a release without a preview; a release that
re-plans at run time instead of applying the confirmed list.

## Decision 4: Maintainerr is read by version (§6.2)

Bunkarr mirrors Maintainerr's collection handler to decide which members it will delete. The
recorded handlers disagree:
- 3.4.1 handles a deleting collection without `deleteAfterDays` at once, and deletes excluded
  members too (probes of its own handler, `testdata/maintainerr/probes`);
- 3.29.0 never handles such a collection, and honours exclusions (`v3.29.0/probes`).

So the client follows the server's version:
- below 3.4.0 the server is refused (the member ids and `overlay-data` differ);
- below 3.27.0 (upstream #3639) a deleting collection with no deletion window is pending, due at
  the member's `addDate`; from 3.27.0 on it deletes nothing;
- below 3.29.0 an excluded member is `undecided` (unknown), because the handler may delete it;
  from 3.29.0 on it is not pending. No recording shows which version between 3.4.1 and 3.29.0
  started honouring exclusions, so versions in between may be unknown needlessly: less precise,
  but safe;
- a version that cannot be parsed counts as the oldest behaviour.

Unknown makes a "pending → skip" rule copy the file and lets a "pending → full" rule win, so both
directions stay protective. Maintainerr's API does not say which Plex server it uses, so each
refresh compares its members with the linked Plex index and marks the facts unknown when they do
not agree (`plexMismatch`). 3.4.1 also answers `rules/exclusion?rulegroupId=N` with every group's
rows; the client keeps the group's rows and the global ones.

**Rejected:** following the design's clauses (they match 3.29) for every version, which reported
3.4.x members as not pending that 3.4.x deletes; refusing Maintainerr below 3.29 (3.4.x is what
many installs run).
