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
ASSETS = ("images/diagram.svg", "videos/demo.mp4", "examples/demo.yaml")


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
chart_version = "{version.removeprefix('v')}"
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
        return (self.output / "content/en" / version / "docs/intro/_index.md").read_text()

    def output_bytes(self):
        return {str(p.relative_to(self.output)): p.read_bytes()
                for p in self.output.rglob("*") if p.is_file()}

    def test_numeric_release_selection_and_minimum(self):
        for series in ("0.9", "0.10", "0.11", "1.9", "1.10"):
            self.add_release(series)
        for tag in ("v0.11.1", "v1.10.2", "v1.10.10", "v99.0.0-rc.1", "migrate"):
            self.git("tag", tag)
        data = self.generate()
        self.assertEqual(data["latest"], "v1.10")
        self.assertEqual([v["label"] for v in data["versions"]],
                         ["v1.10.10", "main", "v1.9.0", "v1.0.0", "v0.11.1", "v0.10.0"])
        self.assertIn('url = "/v1.10/docs/"', (self.output / "hugo.toml").read_text())
        data = self.generate("--min-version", "1.0")
        self.assertEqual([v["key"] for v in data["versions"]], ["v1.10", "main", "v1.9", "v1.0"])

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

    def test_versioned_links_assets_and_content_parameters(self):
        self.git("tag", "v1.0.3")  # Menu label must not overwrite branch install versions.
        for version in self.generate()["versions"]:
            key, commit = version["key"], version["commit"]
            page = self.page(key)
            marker, source_version = ("MAIN", "v99.0.0") if key == "main" else ("BRANCH 1.0", "v1.0.0")
            for asset in ASSETS:
                self.assertIn(f"{key}/{asset}", page)
                self.assertEqual((self.output / "static" / key / asset).read_text(), marker)
            for link in (f"/{key}/docs/guide/?view=full#details", f"/{key}/docs/guide/",
                         f"/{key}/legacy/", "https://example.org/docs/",
                         f"https://raw.githubusercontent.com/kubernetes-sigs/lws/{commit}/docs/examples/demo.yaml"):
                self.assertIn(link, page)
            metadata = json.loads((self.output / "content/en" / key / "_index.md").read_text())
            self.assertEqual(metadata["cascade"]["params"]["version"], source_version)
            self.assertEqual(metadata["cascade"]["params"]["chart_version"], source_version[1:])
        self.assertIn("/v1.0/docs/intro/", (self.output / "static/legacy/index.html").read_text())

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
