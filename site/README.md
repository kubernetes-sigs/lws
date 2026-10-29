# Versioned documentation

The website is authored in `site/content/en/docs`. `docs/examples` contains
workload examples, not a second website. Do not edit `site/_generated`: it is an
ignored build directory, regenerated from Git by
[`snapshot-docs.py`](../hack/releasing/snapshot-docs.py).

## Versions and URLs

- `/stable/docs/...` serves the **latest stable** documentation, read from its
  `release-X.Y` branch, not from the tag.
- `/docs/...` and the latest release's `/vX.Y/docs/...` URLs redirect to
  `/stable/docs/...` with HTTP **301** on Netlify. For example, while v1.0 is
  latest, both `/docs/installation/` and `/v1.0/docs/installation/` redirect to
  `/stable/docs/installation/`. `/` opens the stable overview.
- Older releases keep their `/vX.Y/docs/...` URLs. A new stable minor release
  moves `/stable/` to that release, while patch updates retain the same URLs.
- `/main/docs/...` reads `main` and is clearly marked **Development**, with its
  source commit. The selector lists releases in descending numeric version
  order, then `main` last, separated from the releases by a divider.

Stable `vMAJOR.MINOR.PATCH` tags discover the release series and label each with
its highest patch tag. Prereleases and unrelated tags are ignored. The default
starts at v0.11. For example, the menu shows v1.0.0, then v0.11.1 and main,
not separate entries for v0.11.0 and v0.11.1.
A release branch without a stable tag is not published; a tagged
series without its release branch fails the build rather than showing main's
content under a release label.

Redirects are generated into `site/_generated/static/_redirects`; Netlify
applies them before the HTML fallbacks, preserving query strings. Plain local
static servers use the HTML fallback instead of returning HTTP 301. Permanent
redirects can be cached: a previously visited `/v1.0/` URL may still redirect
to `/stable/` after v1.0 becomes an older release.

The shared theme comes from the current checkout. Each version's page content,
images, videos, and downloadable examples come from its source branch. The
shared branding assets are refreshed from the current checkout. The
resolved commits are recorded in `_generated/data/docs_versions.json`. Raw
GitHub example URLs are pinned to those commits, including examples under
`docs/examples`; source/edit links point to the corresponding branch.

## Generate and preview locally

Requirements: Git, Python 3.9+, Node/npm, Go, and Hugo **extended** 0.152.2.
`make site-server` can install the repository's pinned Hugo binary.

From the repository root:

```sh
npm ci --prefix site
python3 hack/releasing/snapshot-docs.py
hugo server --source site/_generated --destination ../public --cleanDestinationDir
```

Open <http://localhost:1313/>. The script fetches tags, `main`, and release
branches from `kubernetes-sigs/lws` into isolated `refs/docs-snapshots/` refs;
it does not switch branches, alter local tags, or modify tracked website files.
Every successful run replaces the generated site, so removed pages and assets
do not survive from an earlier snapshot. Failed generation preserves the last
successful output.

Useful variants:

```sh
# Regenerate offline using refs already fetched by the script.
python3 hack/releasing/snapshot-docs.py --no-fetch

# Restrict the menu to v1.0 and newer (main remains available).
python3 hack/releasing/snapshot-docs.py --min-version 1.0

# Preview committed documentation changes on your branch as Development.
python3 hack/releasing/snapshot-docs.py --no-fetch --main-ref HEAD

# Fetch from another remote with the required tags and release branches.
python3 hack/releasing/snapshot-docs.py --remote upstream
```

Without cached snapshot refs, offline mode uses local tags and local branches
(then `upstream/` or `origin/` tracking branches). Missing objects in a partial
clone must be fetched before offline generation. Main's displayed SHA always
identifies the content source, not the shared theme's checkout. Uncommitted
content edits are not snapshotted: commit them and use `--main-ref HEAD`, or run
`HUGO_PARAMS_DOCS_COMMIT=$(git rev-parse HEAD) hugo server --source site` for an
unversioned working-copy preview. Uncommitted edits have no chart of their own.

After editing templates, regenerate before reloading the generated preview.
For the Makefile equivalents:

```sh
make site-test
make site-server DOCS_SNAPSHOT_ARGS="--no-fetch"
make site-build
```

## Publication and maintenance

Netlify generates the same ignored directory before each build. No snapshots
or generated version lists need committing. This follows
[Kueue's snapshot approach](https://github.com/kubernetes-sigs/kueue/blob/main/hack/releasing/snapshot-docs.py),
adapted to latest-stable defaults, tag discovery, isolated assets, and ignored
build output instead of committed copies.

Netlify deploy previews and branch deploys use `--main-ref HEAD`: select
`main` to review the PR's documentation changes. Production uses upstream
`main`. The `--cleanDestinationDir` build flag removes stale files from the
generated `site/public` output; do not store hand-written files there.

### When a build runs

| Event | Build | Result |
| --- | --- | --- |
| Merge into `main` | Existing Netlify Git integration builds `main` | Development docs refresh; release snapshots are regenerated too. |
| Merge docs/examples into `release-X.Y` | No automatic production trigger added here | The next **main-based** build updates `/stable/docs/` for the latest release, or `/vX.Y/docs/` for an older release, without a new tag. |
| Publish a stable tag | No automatic production trigger added here | The next **main-based** build refreshes patch labels and the latest-default selection. |
| Open/update a PR | Netlify deploy preview builds the PR checkout | Select `main` to read the PR's docs; production is unchanged. |
| Run the generator locally | No deployment | Preview the same versioned site locally. |

This is one combined website, not a deployment of an old release branch over
production. Always run the current generator and templates from `main`, then
read each version's content from its release branch. A release docs change
needs a **production rebuild**, not merely an isolated Netlify branch preview.

### Release-branch rebuilds

The website is published by Netlify using [`netlify.toml`](../netlify.toml).
Automatic production rebuilds after release-branch changes or new stable tags
are deferred until the Netlify administrators confirm the appropriate setup;
see the [original site setup request](https://github.com/kubernetes/org/issues/5459).
This change adds no deployment workflow or secret requirements. Until those
triggers are configured, release-only changes appear on the next `main`
deployment or after an administrator manually rebuilds `main` in Netlify.

`make site-test` verifies locally that regenerating after a release-branch
update includes the changed documentation without requiring a new tag. It does
not test or configure live deployment triggers.

Tags determine what is **stable**, not whether an image/chart upload has
finished. Publish the release and Helm chart before triggering the production
docs rebuild. Backport documentation fixes to `release-X.Y` and rebuild to
refresh that series; keep unreleased feature documentation on `main`. Do not
backport installation instructions for an unavailable chart.

Main's Helm examples use `0.0.0-dev-<first 7 characters of docs_commit>`, matching
the version selector's source commit. They work when that commit's chart is published;
no website rebuild is needed. PR preview commits normally have no published chart.
Released examples derive their version from the source branch's Hugo `version`.

This implements the versioning tracked in
[#1232](https://github.com/kubernetes-sigs/lws/issues/1232), plus a latest-stable
default (the original issue leaves `/docs/` on main).

## SEO titles

The shared head template combines each page's front-matter title with the site
title, `LeaderWorkerSet & DisaggregatedSet`. For example, the overview becomes
`Distributed Inference APIs | LeaderWorkerSet & DisaggregatedSet`, and the
installation page becomes `Installation | LeaderWorkerSet & DisaggregatedSet`.
No page-specific overrides are needed. This makes browser tabs and search
results clearer, including when readers arrive via llm-d.
