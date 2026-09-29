<!--
Thanks for contributing! Please read CONTRIBUTING.md first. Bunkarr holds people's backups: it
never modifies or deletes source media, and every backup it reports as good must stay restorable.
Correctness and safety come before features. Keep the pull request focused on one change.
Do not include tokens, keys, passwords, recovery kits or real library paths (in code, logs or
screenshots).
-->

## What and why

<!-- What does this change, and why? Link the issue: "Fixes #123" or "Part of #123". -->

## What could go wrong for users' sources or backups?

<!--
Required. Could this change write to a source, drop or corrupt a backed-up file or version, report
a backup as good when it is not, or break a restore without Bunkarr (the recovery kit, plain
files, links.tsv)? Does a job killed in the middle still resume and converge (fault points, the
crash matrix)? Could an unknown or stale fact make a file less protected (tier demotion), or a
release remove more than its preview listed? How does it fail safe when a share is unmounted or
data is missing, stale or ambiguous? If it cannot affect any of this, say so and why.
-->

## How it was tested

<!-- Which tests you added or changed, and anything you checked by hand (the fakes and recorded
responses under testdata/, or the Docker suites). -->

## Checklist

- [ ] `make lint` and `make test` pass, and `make test-e2e` for anything the acceptance suite
      covers (syncs, resume, guards, the API).
- [ ] `make ca-validate` and `go test ./deploy/` pass when `unraid/`, `deploy/` or a workflow
      changes.
- [ ] The Docker suites (`make test-docker`, `make test-offsite`, `make test-engines`) run in CI;
      run the ones your change touches locally if you can.
- [ ] Anything that writes to a destination, retains, expires or releases files has tests for the
      unhappy paths, and a new fault point is in the crash matrix.
- [ ] Security-sensitive changes (authentication, secrets, outbound requests, engine commands,
      the container entrypoint) have a regression test that fails without the change.
- [ ] No new dependencies, or they were agreed in an issue or discussion first.
- [ ] Docs are updated (README, `unraid/README.md`, the design in `docs/design/` when a contract
      changes), and there is a `CHANGELOG.md` entry under *Unreleased*.
- [ ] A new or changed Unraid template setting is called out in that entry: installed containers
      keep their saved template, so the release's `<Changes>` must announce it.
- [ ] UI changes include a screenshot from a demo setup with neutral paths and no real library.
