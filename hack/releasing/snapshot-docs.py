#!/usr/bin/env python3
# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Generate an untracked, versioned Hugo site from release branches.

Adapted from kubernetes-sigs/kueue/hack/releasing/snapshot-docs.py.
Unlike Kueue, do not commit snapshots or edit the source Hugo configuration.
Stable tags discover/order release series; release-X.Y supplies their content.
The latest release is the default, followed by main and older release series.

Requires Python 3.9+ and Git. Run --help for local/offline generation options.
"""

import argparse
import html
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile


REPO_ROOT = Path(__file__).resolve().parents[2]
REMOTE = "https://github.com/kubernetes-sigs/lws.git"
CACHE = "refs/docs-snapshots"
MARKER = ".snapshot-docs-generated"
STABLE_TAG = re.compile(r"v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$")


def git(repo, *args):
    # In a partial clone --no-fetch must not quietly initiate lazy blob fetches.
    env = dict(os.environ, GIT_NO_LAZY_FETCH="1")
    return subprocess.run(
        ["git", *args], cwd=repo, check=True, capture_output=True, env=env
    ).stdout.decode()


def fetch_refs(repo, remote):
    subprocess.run(
        ["git", "fetch", "--no-tags", "--prune", remote,
         f"+refs/heads/main:{CACHE}/heads/main",
         f"+refs/heads/release-*:{CACHE}/heads/release-*",
         f"+refs/tags/v*:{CACHE}/tags/v*"],
        cwd=repo, check=True,
    )


def resolve_branch(repo, branch, cached):
    refs = ([f"{CACHE}/heads/{branch}"] if cached else
            [f"refs/heads/{branch}", f"refs/remotes/upstream/{branch}",
             f"refs/remotes/origin/{branch}"])
    for ref in refs:
        try:
            return git(repo, "rev-parse", "--verify", f"{ref}^{{commit}}").strip()
        except subprocess.CalledProcessError:
            pass
    raise ValueError(
        f"Missing source branch {branch}. Fetch it (or rerun without --no-fetch). "
        "A tag or main will never be substituted for a missing release branch."
    )


def discover_versions(repo, minimum, main_ref=None):
    cached = bool(git(repo, "for-each-ref", "--format=%(refname)", CACHE).strip())
    prefix = f"{CACHE}/tags/" if cached else "refs/tags/"
    tags = git(repo, "for-each-ref", "--format=%(refname)", prefix).splitlines()
    series = {}
    for ref in tags:
        tag = ref.removeprefix(prefix)
        match = STABLE_TAG.fullmatch(tag)
        if not match:
            continue
        numbers = tuple(map(int, match.groups()))
        if numbers[:2] >= minimum and numbers > series.get(numbers[:2], (-1, -1, -1)):
            series[numbers[:2]] = numbers
    if not series:
        raise ValueError("No stable release tags found at or above --min-version. Fetch tags first.")
    versions = []
    for major, minor, patch in sorted(series.values(), reverse=True):
        key = f"v{major}.{minor}"
        branch = f"release-{major}.{minor}"
        versions.append(dict(key=key, label=f"{key}.{patch}", branch=branch,
                             commit=resolve_branch(repo, branch, cached),
                             url=f"/{key}/docs/", latest=not versions))
    main_commit = (git(repo, "rev-parse", "--verify", f"{main_ref}^{{commit}}").strip()
                   if main_ref else resolve_branch(repo, "main", cached))
    versions.insert(1, dict(key="main", label="main", branch="main", commit=main_commit,
                            url="/main/docs/", latest=False))
    return dict(latest=versions[0]["key"], versions=versions)


def extract_sources(repo, commit, destination, offline):
    """Read only content, assets and config; never execute a historical script."""
    wanted = ["site/content", "site/static", "site/hugo.toml"]
    paths = [p for p in wanted if git(repo, "ls-tree", "--name-only", commit, "--", p).strip()]
    archive = subprocess.run(
        ["git", "archive", commit, "--", *paths], cwd=repo, check=True,
        capture_output=True, env=dict(os.environ, GIT_NO_LAZY_FETCH="1" if offline else "0"),
    ).stdout
    with tarfile.open(fileobj=io.BytesIO(archive)) as tar:
        for member in tar:
            path = PurePosixPath(member.name)
            if path.is_absolute() or ".." in path.parts or not (member.isdir() or member.isfile()):
                raise ValueError(f"Unsafe documentation archive entry: {member.name}")
            target = destination.joinpath(*path.parts)
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                with tar.extractfile(member) as source, target.open("wb") as output:
                    shutil.copyfileobj(source, output)


def split_front_matter(text):
    if text.startswith("---\n"):
        match = re.search(r"\n---(?:\n|$)", text[4:])
        if not match:
            raise ValueError("Unterminated YAML front matter")
        return text[4:][:match.start()], text[4:][match.end():]
    if text.startswith(("+++", "{")):
        raise ValueError("Snapshot rewriting currently requires YAML front matter")
    return "", text


def prefix_aliases(front, key):
    # Support the YAML block and inline-list forms used by Hugo content.
    def alias(value):
        raw = value.strip().strip("\"'")
        if not raw.startswith("/") or raw.startswith("//") or ".." in PurePosixPath(raw).parts:
            raise ValueError(f"Expected a site-relative alias, got {value}")
        return json.dumps(f"/{key}{raw}")

    out, in_aliases = [], False
    for line in front.splitlines():
        if re.match(r"^aliases:\s*\[", line):
            values = line.split("[", 1)[1].rsplit("]", 1)[0]
            out.append("aliases: [" + ", ".join(alias(v) for v in values.split(",") if v.strip()) + "]")
            continue
        if re.fullmatch(r"aliases:\s*", line):
            in_aliases = True
            out.append(line)
            continue
        if in_aliases and re.match(r"^\s*-\s+", line):
            out.append("  - " + alias(re.sub(r"^\s*-\s+", "", line)))
            continue
        in_aliases = False
        out.append(line)
    return "\n".join(out)


def original_aliases(front):
    aliases, in_aliases = [], False
    for line in front.splitlines():
        if re.match(r"^aliases:\s*\[", line):
            aliases.extend(v.strip().strip("\"'") for v in
                           line.split("[", 1)[1].rsplit("]", 1)[0].split(",") if v.strip())
        elif re.fullmatch(r"aliases:\s*", line):
            in_aliases = True
        elif in_aliases and re.match(r"^\s*-\s+", line):
            aliases.append(re.sub(r"^\s*-\s+", "", line).strip().strip("\"'"))
        else:
            in_aliases = False
    return aliases


def rewrite_page(text, version, asset_roots):
    key = version["key"]
    front, body = split_front_matter(text)
    # Menus belong to the shared site shell, not each release's copy.
    front = re.sub(r"^menu:[^\n]*(?:\n[ \t]+[^\n]*)*\n?", "", front, flags=re.MULTILINE)
    front = prefix_aliases(front, key)
    # Explicit URLs otherwise escape the version namespace.
    front = re.sub(r"^url:\s*[\"']?(/[^\"'\n]*)[\"']?\s*$",
                   lambda m: "url: " + json.dumps(f"/{key}{m[1].strip()}"), front, flags=re.MULTILINE)
    roots = "|".join(re.escape(root) for root in sorted({"docs", *asset_roots}))
    # Convert absolute links to this website first, then namespace local links.
    body = re.sub(r"https?://lws\.sigs\.k8s\.io(?=/(?:" + roots + r")(?:[/#?\s\"')>]|$))", "", body)
    body = re.sub(r"(?P<before>[\s(\"'=<>])/(?P<root>" + roots + r")(?=[/#?\s\"')>]|$)",
                  lambda m: f"{m['before']}/{key}/{m['root']}", body)
    body = re.sub(r"(\{\{[<%]\s*include\s+(?:file=)?[\"'])/?([^\"']+)",
                  lambda m: f"{m[1]}{key}/{m[2]}", body)
    # /docs contains executable examples, not another Hugo tree. Pin their raw
    # URLs to the exact branch snapshot so commands cannot drift after a build.
    body = re.sub(r"(https://raw\.githubusercontent\.com/kubernetes-sigs/lws/)(?:refs/heads/)?main/",
                  lambda m: f"{m[1]}{version['commit']}/", body)
    body = re.sub(r"(https://github\.com/kubernetes-sigs/lws/(?:blob|tree)/)main/",
                  lambda m: f"{m[1]}{version['commit']}/", body)
    return f"---\n{front}\n---\n{body}" if front else body


def release_params(source):
    """Preserve the branch's content parameters, independently of menu labels."""
    config = source / "site/hugo.toml"
    params, in_params = {}, False
    if not config.is_file():
        return params
    for line in config.read_text().splitlines():
        if line.strip().startswith("["):
            in_params = line.strip() == "[params]"
        if in_params:
            match = re.fullmatch(r'\s*(version|chart_version)\s*=\s*"([^"]*)"\s*(?:#.*)?', line)
            if match:
                params[match[1]] = match[2]
    return params


def page_route(path, docs):
    relative = path.relative_to(docs)
    if path.stem in ("_index", "index"):
        route = "docs/" + (relative.parent.as_posix() + "/" if relative.parent != Path(".") else "")
    else:
        route = "docs/" + relative.with_suffix("").as_posix() + "/"
    # Hugo's default URL path normalizes whitespace and letter case.
    return re.sub(r"\s+", "-", route).lower()


def write_redirect(root, route, target, label="Latest stable documentation"):
    # Redirects are ordinary static HTML: work on Netlify AND any local server.
    # JS preserves fragments/queries; meta refresh + link also work without JS.
    destination = root / route.strip("/") / "index.html"
    destination.parent.mkdir(parents=True, exist_ok=True)
    title = ("Distributed Inference APIs | LeaderWorkerSet & DisaggregatedSet"
             if target.endswith("/docs/overview/") else label)
    destination.write_text(
        '<!doctype html><html lang="en"><meta charset="utf-8">'
        '<meta name="robots" content="noindex">'
        f'<link rel="canonical" href="{html.escape(target, quote=True)}">'
        f'<meta http-equiv="refresh" content="0; url={html.escape(target, quote=True)}">'
        f'<title>{html.escape(title)}</title><script>location.replace({json.dumps(target)}'
        '+location.search+location.hash);</script>'
        f'<a href="{html.escape(target, quote=True)}">{html.escape(label)}</a></html>\n'
    )


def generate(repo, manifest, output, offline=False):
    site = repo / "site"
    if output.is_symlink() or (output.exists() and not (output / MARKER).is_file()):
        raise ValueError(f"Refusing to replace {output}: not a recognized generated site")
    with tempfile.TemporaryDirectory(prefix=".docs-snapshot-", dir=site) as temp:
        staging = Path(temp) / "generated"
        staging.mkdir()
        # Current presentation, not the old branch's theme or executable code.
        for name in ("hugo.toml", "go.mod", "go.sum", "package.json", "package-lock.json",
                     "layouts", "assets", "archetypes", "i18n", "content", "static", "data"):
            source = site / name
            if source.is_symlink() or (source.is_dir() and any(p.is_symlink() for p in source.rglob("*"))):
                raise ValueError(f"Refusing symlink in website source: {source}")
            if source.is_dir():
                shutil.copytree(source, staging / name)
            elif source.is_file():
                shutil.copy2(source, staging / name)
        for docs in (staging / "content").glob("*/docs"):
            shutil.rmtree(docs)
        routes, latest_routes, latest_aliases = set(), set(), {}
        for version in manifest["versions"]:
            key = version["key"]
            source = Path(temp) / key
            extract_sources(repo, version["commit"], source, offline)
            docs_trees = sorted((source / "site/content").glob("*/docs"))
            if not docs_trees or not any((p / "_index.md").is_file() for p in docs_trees):
                raise ValueError(f"{version['branch']} has no website docs; choose a newer --min-version")
            static = source / "site/static"
            roots = {p.name for p in static.iterdir()} if static.is_dir() else set()
            if static.is_dir():
                shutil.copytree(static, staging / "static" / key)
            for docs in docs_trees:
                locale = docs.parent.name
                destination = staging / "content" / locale / key / "docs"
                shutil.copytree(docs, destination)
                for page in sorted(destination.rglob("*")):
                    if page.suffix not in (".md", ".html") or not page.is_file():
                        continue
                    original = page.read_text()
                    try:
                        page.write_text(rewrite_page(original, version, roots))
                    except ValueError as error:
                        raise ValueError(f"{version['branch']}:{page.relative_to(destination)}: {error}") from error
                    if locale == "en":
                        route = page_route(page, destination)
                        routes.add(route)
                        if version["latest"]:
                            latest_routes.add(route)
                            front, _ = split_front_matter(original)
                            for alias in original_aliases(front):
                                latest_aliases[alias] = f"/{key}/{route}"
                # Separate parent cascade avoids overwriting existing page params
                # or cascades in branch content. Docsy needs type: docs here.
                params = dict(docs_version=key, docs_branch=version["branch"],
                              docs_commit=version["commit"], docs_source_prefix=key,
                              githubbranch=version["branch"], github_branch=version["branch"],
                              path_base_for_github_subdir=dict(
                                  **{"from": f"^content/{locale}/{re.escape(key)}/", "to": f"content/{locale}/"}))
                params.update(release_params(source))
                metadata = dict(title=version["label"], type="docs", params=params,
                                cascade=dict(type="docs", params=params),
                                build=dict(render="link", list="never"))
                (destination.parent / "_index.md").write_text(json.dumps(metadata, indent=2) + "\n")
                # Branch metadata can still expose main via site-level shortcodes;
                # .Page.Param, used by the local params shortcode, sees this cascade.
            write_redirect(staging / "static", key, version["url"],
                           label="Development documentation" if key == "main" else f"{version['label']} documentation")
            print(f"{version['label']}: {version['branch']} @ {version['commit'][:12]}")
        # Copying the site shell preserves its non-docs pages. The root homepage
        # and /docs bookmarks now route to the latest stable release, never main.
        latest = manifest["latest"]
        latest_home = f"/{latest}/docs/overview/" if "docs/overview/" in latest_routes else f"/{latest}/docs/"
        for route in routes:
            target = f"/{latest}/{route}" if route in latest_routes else f"/{latest}/docs/"
            write_redirect(staging / "static", route, target)
        for alias, target in latest_aliases.items():
            write_redirect(staging / "static", alias, target)
        write_redirect(staging / "static", "", latest_home)
        # Avoid two owners for /index.html (a Hugo homepage and a static redirect).
        for home in (staging / "content/en/_index.html", staging / "content/en/_index.md"):
            if home.exists():
                home.write_text('---\nbuild:\n  render: link\n  list: never\n---\n')
        (staging / "data").mkdir(exist_ok=True)
        (staging / "data/docs_versions.json").write_text(json.dumps(manifest, indent=2) + "\n")
        # No version copies should register their own global navbar entries.
        config_file = staging / "hugo.toml"
        config_file.write_text(re.sub(r"(?m)^enableGitInfo\s*=.*$", "enableGitInfo = false", config_file.read_text()))
        with config_file.open("a") as config:
            config.write(f'\n[[menu.main]]\n  name = "Documentation"\n  weight = 20\n  url = "{latest_home}"\n')
        (staging / MARKER).write_text("Generated by hack/releasing/snapshot-docs.py; do not edit.\n")
        # Only replace a successfully generated, explicitly marked directory.
        old = Path(temp) / "previous"
        if output.exists():
            output.rename(old)
        try:
            staging.rename(output)
        except OSError:
            if old.exists():
                old.rename(output)
            raise
    print(f"Generated {output}; build with hugo --source {output}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, default=REPO_ROOT, help=argparse.SUPPRESS)
    parser.add_argument("--remote", default=REMOTE, help="Git remote name or URL to fetch (default: upstream LWS URL)")
    parser.add_argument("--no-fetch", action="store_true", help="Use cached snapshot refs, or local tags/branches if no cache exists")
    parser.add_argument("--min-version", default="1.0", help="Oldest minor series to include, e.g. 0.11 (default: 1.0)")
    parser.add_argument("--main-ref", help="Override main content source with a local ref (e.g. HEAD for a docs preview)")
    args = parser.parse_args()
    try:
        if not re.fullmatch(r"v?\d+\.\d+", args.min_version):
            raise ValueError("--min-version must be a major.minor version, e.g. 1.0")
        repo = args.repo.resolve()
        if not args.no_fetch:
            fetch_refs(repo, args.remote)
        minimum = tuple(map(int, args.min_version.removeprefix("v").split(".")))
        manifest = discover_versions(repo, minimum, args.main_ref)
        generate(repo, manifest, repo / "site/_generated", offline=args.no_fetch)
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        print(f"snapshot-docs: {error}", file=sys.stderr)
        if isinstance(error, subprocess.CalledProcessError) and error.stderr:
            print(error.stderr.decode() if isinstance(error.stderr, bytes) else error.stderr, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
