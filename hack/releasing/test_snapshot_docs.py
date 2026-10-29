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

"""Offline snapshot generation checks using disposable Git repositories."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("snapshot-docs.py")
MAKEFILE = SCRIPT.resolve().parents[2] / "Makefile"
ASSETS = ("images/diagram.svg", "videos/demo.mp4", "examples/demo.yaml")
BRANDING = ("images/lws-ds-logos.svg", "images/ds-concept.svg", "images/ds-logo.svg")


class SnapshotDocsTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="lws-docs-test-")
        self.addCleanup(temporary.cleanup)
        self.repo = Path(temporary.name)
        self.output = self.repo / "site/_generated"
        self.env = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull,
                        GIT_ALLOW_PROTOCOL="file", GIT_AUTHOR_NAME="Docs Fixture",
                        GIT_COMMITTER_NAME="Docs Fixture", GIT_AUTHOR_EMAIL="fixture@example.invalid",
                        GIT_COMMITTER_EMAIL="fixture@example.invalid")
        self.git("init", "-q", "-b", "main")
        self.write("site/.gitignore", "_generated/\n")
        self.write("site/postcss.config.js", 'module.exports = {plugins: [require("autoprefixer")]};\n')
        self.write_site("MAIN", "v99.0.0")
        self.commit("Main documentation")
        self.add_release("1.0")

    def git(self, *args):
        return subprocess.run(["git", *args], cwd=self.repo, env=self.env,
                              capture_output=True, text=True, check=True).stdout.strip()

    def write(self, relative, text):
        path = self.repo / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)

    def commit(self, message):
        self.git("add", ".")
        self.git("commit", "-qm", message)

    def write_site(self, marker, version):
        self.write("site/hugo.toml", f'''baseURL = "/"
[params]
version = "{version}"
''')
        self.write("site/content/en/docs/_index.md", "---\ntitle: Documentation\n---\n")
        self.write("site/content/en/docs/intro/_index.md", '''---
title: Introduction
aliases: [/legacy/]
---
''' + marker + '''
[Guide](/docs/guide/?view=full#details)
[Absolute](https://lws.sigs.k8s.io/docs/guide/)
[External](https://example.org/docs/)
![Diagram](/images/diagram.svg)
<video src="/videos/demo.mp4"></video>
{{< include "examples/demo.yaml" "yaml" >}}
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/main/docs/examples/demo.yaml
''')
        self.write("site/content/en/docs/guide/_index.md", "---\ntitle: Guide\n---\n")
        for asset in ASSETS:  # Dummy bytes: no media decoding is involved.
            self.write("site/static/" + asset, marker)

    def add_release(self, series):
        self.git("switch", "-qc", "release-" + series, "main")
        self.write_site("TAG " + series, "v" + series + ".0")
        self.commit("Tagged " + series)
        self.git("tag", "v" + series + ".0")
        self.write_site("BRANCH " + series, "v" + series + ".0")
        self.commit("Documentation fix after tag")
        self.git("switch", "-q", "main")

    def generate(self, *args, success=True):
        result = subprocess.run([sys.executable, str(SCRIPT), "--repo", str(self.repo),
                                 "--no-fetch", *args], env=self.env, capture_output=True,
                                text=True, timeout=30)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        return json.loads((self.output / "data/docs_versions.json").read_text()) if success else None

    def page(self, version):
        manifest = json.loads((self.output / "data/docs_versions.json").read_text())
        prefix = next(v["prefix"] for v in manifest["versions"] if v["key"] == version)
        return (self.output / "content/en" / prefix / "docs/intro/_index.md").read_text()

    def output_bytes(self):
        return {str(p.relative_to(self.output)): p.read_bytes()
                for p in self.output.rglob("*") if p.is_file()}

    def test_numeric_release_selection_and_minimum(self):
        for series in ("0.9", "0.11", "1.9", "1.10"):
            self.add_release(series)
        for tag in ("v0.11.1", "v1.10.2", "v1.10.10", "v99.0.0-rc.1", "migrate"):
            self.git("tag", tag)
        data = self.generate()
        self.assertEqual(data["latest"], "v1.10")
        self.assertEqual([v["label"] for v in data["versions"]],
                         ["v1.10.10", "v1.9.0", "v1.0.0", "v0.11.1", "main"])
        self.assertEqual(data["versions"][0]["prefix"], "stable")
        self.assertEqual(data["versions"][0]["url"], "/stable/docs/")
        self.assertIn('url = "/stable/docs/"', (self.output / "hugo.toml").read_text())
        data = self.generate("--min-version", "1.0")
        self.assertEqual([v["key"] for v in data["versions"]], ["v1.10", "v1.9", "v1.0", "main"])

    def test_release_branch_updates_without_a_new_tag(self):
        before = self.generate()["versions"][0]
        main_page = self.page("main")
        self.assertIn("BRANCH 1.0", self.page("v1.0"))
        self.assertNotIn("TAG 1.0", self.page("v1.0"))
        self.git("switch", "-q", "release-1.0")
        self.write_site("MERGED DOCUMENTATION FIX", "v1.0.0")
        self.commit("Merge after publication")
        updated_commit = self.git("rev-parse", "HEAD")
        self.git("switch", "-q", "main")
        after = self.generate()["versions"][0]
        self.assertEqual(after, dict(before, commit=updated_commit))
        self.assertNotEqual(after["commit"], before["commit"])
        self.assertIn("MERGED DOCUMENTATION FIX", self.page("v1.0"))
        self.assertEqual(self.page("main"), main_page)
        self.add_release("1.1")
        data = self.generate()
        self.assertEqual(data["latest"], "v1.1")
        self.assertEqual(data["versions"][0]["prefix"], "stable")
        self.assertIn("BRANCH 1.1", self.page("v1.1"))
        self.assertIn("MERGED DOCUMENTATION FIX", self.page("v1.0"))
        self.assertTrue((self.output / "content/en/v1.0/docs/intro/_index.md").is_file())
        historical = next(v for v in data["versions"] if v["key"] == "v1.0")
        self.assertEqual(historical["url"], "/v1.0/docs/")
        redirects = (self.output / "static/_redirects").read_text()
        self.assertIn("/v1.1/* /stable/:splat 301!", redirects)
        self.assertNotIn("/v1.0/*", redirects)

    def test_versioned_links_assets_and_content_parameters(self):
        self.git("tag", "v1.0.3")  # Menu label must not overwrite branch install versions.
        self.git("switch", "-q", "release-1.0")
        for asset in BRANDING:
            self.write("site/static/" + asset, "ARCHIVED BRANDING")
        self.commit("Original release branding")
        self.git("switch", "-q", "main")
        for asset in BRANDING:
            self.write("site/static/" + asset, "CURRENT BRANDING")
        for version in self.generate()["versions"]:
            key, prefix, commit = version["key"], version["prefix"], version["commit"]
            page = self.page(key)
            marker, source_version = ("MAIN", "v99.0.0") if key == "main" else ("BRANCH 1.0", "v1.0.0")
            for asset in ASSETS:
                self.assertIn(f"{prefix}/{asset}", page)
                self.assertEqual((self.output / "static" / prefix / asset).read_text(), marker)
            for asset in BRANDING:
                self.assertEqual((self.output / "static" / prefix / asset).read_text(), "CURRENT BRANDING")
            for link in (f"/{prefix}/docs/guide/?view=full#details", f"/{prefix}/docs/guide/",
                         f"/{prefix}/legacy/", "https://example.org/docs/",
                         f"https://raw.githubusercontent.com/kubernetes-sigs/lws/{commit}/docs/examples/demo.yaml"):
                self.assertIn(link, page)
            metadata = json.loads((self.output / "content/en" / prefix / "_index.md").read_text())
            self.assertEqual(metadata["cascade"]["params"]["docs_version"], key)
            self.assertEqual(metadata["cascade"]["params"]["docs_commit"], commit)
            self.assertEqual(metadata["cascade"]["params"]["docs_source_prefix"], prefix)
            self.assertEqual(metadata["cascade"]["params"]["version"], source_version)
        for route in ("docs/intro", "v1.0/docs/intro", "legacy", "v1.0/legacy"):
            fallback = (self.output / "static" / route / "index.html").read_text()
            self.assertIn("/stable/docs/intro/", fallback)
            self.assertIn("+location.search+location.hash", fallback)
        redirects = (self.output / "static/_redirects").read_text()
        for source, target in (("/docs/intro/", "/stable/docs/intro/"),
                               ("/v1.0/docs/intro/", "/stable/docs/intro/"),
                               ("/legacy/", "/stable/docs/intro/"),
                               ("/v1.0/legacy/", "/stable/docs/intro/"),
                               ("/docs/*", "/stable/docs/:splat"),
                               ("/v1.0/*", "/stable/:splat")):
            self.assertIn(f"{source} {target} 301!", redirects)
        self.assertNotIn("/stable/*", redirects)
        self.assertNotIn("/main/*", redirects)
        self.assertEqual((self.output / "postcss.config.js").read_text(),
                         (self.repo / "site/postcss.config.js").read_text())

    def test_release_preparation_selects_branch_documentation_target(self):
        self.write("go.mod", "module fixture.invalid/docs\n\ngo 1.20\n")
        config = '''[params]
version = "v99.0.0"
githubbranch = "main"
historical_version = "v1.1.0"
[other]
version = "v2.0.0"
'''
        self.write("site/hugo.toml", config)
        installation_path = "site/content/en/docs/installation/_index.md"
        installation = (MAKEFILE.parent / installation_path).read_text()
        # Release versions and exact-commit development versions are rendered by Hugo.
        self.assertNotRegex(installation, r"(?:CHART_)?VERSION=v?\d+\.\d+\.\d+(?=\s|$)")
        self.assertIn('VERSION={{< params "version" >}}', installation)
        self.assertIn('CHART_VERSION={{< chart-version >}}', installation)
        self.assertEqual(installation.count('CHART_VERSION={{< dev-chart-version >}}'), 2)
        self.assertNotIn("0.0.0-main", installation)
        self.write(installation_path, installation)
        chart = 'appVersion: v1.0.0\nversion: 0.0.0-master\n'
        self.write("charts/lws/Chart.yaml", chart)
        for branch, release, development, expected in (
                ("main", "1.0.0", "1.1.0", "1.1.0"),
                ("release-1.0", "1.0.1", "1.1.0", "1.0.1"),
                ("release-1.1", "1.1.3", "1.2.0", "1.1.3"),
                ("main", "1.1.0", "1.2.0", "1.2.0")):
            with self.subTest(branch=branch, release=release, development=development):
                for _ in range(2):  # Repeated preparation must be idempotent.
                    result = subprocess.run(["make", "-s", "-I", str(MAKEFILE.parent),
                                             "-f", str(MAKEFILE), "prepare-docs-release",
                                             f"RELEASE_BRANCH={branch}",
                                             f"RELEASE_VERSION=v{release}",
                                             f"DEVELOPMENT_DOCS_VERSION=v{development}"],
                                            cwd=self.repo, env=self.env, capture_output=True,
                                            text=True, timeout=30)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertEqual((self.repo / "site/hugo.toml").read_text(),
                                     config.replace("99.0.0", expected).replace(
                                         'githubbranch = "main"', f'githubbranch = "{branch}"'))
                    self.assertEqual((self.repo / installation_path).read_text(), installation)
                    self.assertEqual((self.repo / "charts/lws/Chart.yaml").read_text(), chart)

    def test_offline_cache_isolation_and_main_preview(self):
        for destination, source in (("heads/main", "main"), ("heads/release-1.0", "release-1.0"),
                                    ("tags/v1.0.0", "v1.0.0")):
            self.git("update-ref", "refs/docs-snapshots/" + destination, source)
        self.add_release("9.0")
        self.write_site("LOCAL PREVIEW", "v99.0.0")
        self.commit("Unfetched local changes")
        self.assertEqual([v["key"] for v in self.generate()["versions"]], ["v1.0", "main"])
        self.assertIn("MAIN", self.page("main"))
        self.generate("--main-ref", "HEAD")
        self.assertIn("LOCAL PREVIEW", self.page("main"))
        self.assertIn("BRANCH 1.0", self.page("v1.0"))
        previous = self.output_bytes()
        self.git("update-ref", "-d", "refs/docs-snapshots/heads/release-1.0")
        self.generate(success=False)  # A local branch cannot replace a missing cached source.
        self.assertEqual(self.output_bytes(), previous)

    def test_user_owned_output_and_output_symlinks_are_preserved(self):
        self.write("site/_generated/keep.txt", "User-owned output")
        self.generate(success=False)
        protected = self.repo / "protected"
        self.output.rename(protected)
        self.output.symlink_to(protected, target_is_directory=True)
        self.generate(success=False)
        self.assertTrue(self.output.is_symlink())
        self.assertEqual((protected / "keep.txt").read_text(), "User-owned output")

    def test_source_symlinks_cannot_publish_or_replace_previous_output(self):
        self.generate()
        previous = self.output_bytes()
        self.write("private.txt", "Not website content")
        (self.repo / "site/static/leak").symlink_to(self.repo / "private.txt")
        self.generate(success=False)  # Current working-tree shell.
        self.assertEqual(self.output_bytes(), previous)
        self.git("switch", "-q", "release-1.0")
        self.commit("Unsafe historical asset")
        self.git("switch", "-q", "main")
        self.generate(success=False)  # Archived release content, after staging has begun.
        self.assertEqual(self.output_bytes(), previous)


if __name__ == "__main__":
    unittest.main()
