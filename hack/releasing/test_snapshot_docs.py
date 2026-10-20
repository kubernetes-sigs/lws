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

"""Offline, black-box checks for snapshot-docs.py using disposable Git repos."""

import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("snapshot-docs.py")


class SnapshotDocsTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="lws-docs-test-")
        self.addCleanup(temporary.cleanup)
        self.repo = Path(temporary.name)
        self.output = self.repo / "site/_generated"
        self.env = dict(os.environ, GIT_CONFIG_NOSYSTEM="1",
                        GIT_CONFIG_GLOBAL=os.devnull, GIT_ALLOW_PROTOCOL="file",
                        GIT_AUTHOR_NAME="Docs Fixture", GIT_COMMITTER_NAME="Docs Fixture",
                        GIT_AUTHOR_EMAIL="fixture@example.invalid",
                        GIT_COMMITTER_EMAIL="fixture@example.invalid")
        self.git("init", "-q", "-b", "main")
        self.write("site/.gitignore", "_generated/\n")
        self.write("site/go.mod", "module example.invalid/docs\n\ngo 1.19\n")
        self.write("unrelated.txt", "Do not change this file.\n")
        self.write_site("MAIN CONTENT", "v99.0.0")
        self.commit("Main documentation")
        self.add_release("1.0")

    def git(self, *args):
        result = subprocess.run(["git", *args], cwd=self.repo, env=self.env,
                                capture_output=True, text=True, check=True)
        return result.stdout.strip()

    def write(self, relative, text):
        path = self.repo / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")

    def commit(self, message):
        self.git("add", ".")
        self.git("commit", "-qm", message)

    def write_site(self, marker, version):
        self.write("site/hugo.toml", f'''baseURL = "/"
title = "Fixture"
contentDir = "content/en"
[params]
version = "{version}"
githubbranch = "main"
github_repo = "https://github.com/kubernetes-sigs/lws"
''')
        self.write("site/content/en/_index.html", "---\ntitle: Home\n---\nHome\n")
        self.write("site/content/en/docs/_index.md", '''---
title: Documentation
menu:
  main:
    weight: 20
---
''' + marker + "\n")
        self.write("site/content/en/docs/intro/_index.md", '''---
title: Introduction
aliases:
  - /docs/old-intro/
  - /legacy/
---
''' + marker + '''
[Local guide](/docs/guide/?view=full#details)
[Absolute guide](https://lws.sigs.k8s.io/docs/guide/#details)
[Other website](https://example.org/docs/guide/)
[Relative guide](../guide/#details)
![Diagram](/images/diagram.svg)
<video poster="/images/poster.png"><source src="/videos/demo.mp4" type="video/mp4"></video>
[Download example](/examples/demo.yaml)
{{< include "examples/demo.yaml" "yaml" >}}
{{< include file="examples/demo.yaml" lang="yaml" >}}

```sh
curl https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/heads/main/docs/examples/demo.yaml
```
''')
        self.write("site/content/en/docs/guide/_index.md", "---\ntitle: Guide\n---\n## Details\n" + marker + "\n")
        # The generator copies assets; these fixture bytes need not be decodable media.
        for path in ("images/diagram.svg", "images/poster.png", "videos/demo.mp4", "examples/demo.yaml"):
            self.write("site/static/" + path, marker + "\n")
        self.write("docs/examples/demo.yaml", marker + "\n")

    def add_release(self, series):
        branch = "release-" + series
        self.git("switch", "-qc", branch, "main")
        self.write_site("TAG CONTENT " + series, "v" + series + ".0")
        self.commit("Tagged " + series)
        self.git("tag", "v" + series + ".0")
        self.write_site("BRANCH CONTENT " + series, "v" + series + ".0")
        self.commit("Documentation fix after tag " + series)
        self.git("switch", "-q", "main")

    def generate(self, *args, success=True):
        result = subprocess.run([sys.executable, str(SCRIPT), "--repo", str(self.repo),
                                 "--no-fetch", *args], cwd=self.repo, env=self.env,
                                capture_output=True, text=True, timeout=30)
        if success:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        return result

    def manifest(self):
        return json.loads((self.output / "data/docs_versions.json").read_text())

    def page(self, version, name="intro"):
        return self.output / "content/en" / version / "docs" / name / "_index.md"

    @staticmethod
    def file_bytes(root, exclude=()):
        result = {}
        for path in root.rglob("*"):
            relative = path.relative_to(root)
            if any(relative == Path(item) or Path(item) in relative.parents for item in exclude):
                continue
            if path.is_file():
                result[str(relative)] = path.read_bytes()
        return result

    def assert_default_redirect(self, suffix, latest):
        target = "/" + latest + "/docs/" + suffix
        redirect = self.output / "static/docs" / suffix / "index.html"
        if redirect.exists():
            html = redirect.read_text()
            self.assertIn(target, html)
            self.assertRegex(html.lower(), r"http-equiv\s*=\s*['\"]refresh['\"]")
            return
        # Hugo aliases generate equivalent redirects during the site build.
        frontmatter = self.page(latest, suffix).read_text().split("---", 2)[1]
        alias = "/docs/" + suffix
        self.assertRegex(frontmatter, r"(?m)^\s*-\s*['\"]?" + re.escape(alias) + r"['\"]?\s*$")

    def test_branch_tip_is_used_instead_of_tag(self):
        self.generate()
        page = self.page("v1.0").read_text()
        self.assertIn("BRANCH CONTENT 1.0", page)
        self.assertNotIn("TAG CONTENT", page)
        self.assertIn("MAIN CONTENT", self.page("main").read_text())
        data = self.manifest()
        self.assertEqual(data["latest"], "v1.0")
        self.assertEqual([entry["key"] for entry in data["versions"]], ["v1.0", "main"])
        for entry in data["versions"]:
            self.assertTrue({"key", "label", "url", "branch", "commit", "latest"} <= entry.keys())
            branch = "main" if entry["key"] == "main" else "release-1.0"
            self.assertEqual(entry["branch"], branch)
            self.assertEqual(entry["commit"], self.git("rev-parse", branch))
            self.assertEqual(entry["url"], "/" + entry["key"] + "/docs/")
            self.assertEqual(entry["latest"], entry["key"] == "v1.0")

    def test_patch_tags_group_and_versions_sort_numerically(self):
        self.add_release("1.9")
        self.add_release("1.10")
        self.git("tag", "v1.10.2", "release-1.10")
        self.git("tag", "v1.10.10", "release-1.10")
        self.generate()
        data = self.manifest()
        self.assertEqual(data["latest"], "v1.10")
        self.assertEqual([entry["key"] for entry in data["versions"]], ["v1.10", "main", "v1.9", "v1.0"])

    def test_content_version_parameters_come_from_the_source_branch(self):
        self.git("switch", "-q", "release-1.0")
        self.write("site/hugo.toml", '''baseURL = "/"
[params]
version = "v1.0.0"
chart_version = "0.6.2"
''')
        self.commit("Release-specific install versions")
        self.git("tag", "v1.0.3")
        self.git("switch", "-q", "main")
        self.generate()
        self.assertEqual(self.manifest()["versions"][0]["label"], "v1.0.3")
        for key, version in (("v1.0", "v1.0.0"), ("main", "v99.0.0")):
            metadata = json.loads((self.output / "content/en" / key / "_index.md").read_text())
            self.assertEqual(metadata["params"]["version"], version)
            self.assertEqual(metadata["cascade"]["params"]["version"], version)
            if key == "v1.0":
                self.assertEqual(metadata["params"]["chart_version"], "0.6.2")
                self.assertEqual(metadata["cascade"]["params"]["chart_version"], "0.6.2")

    def test_prereleases_and_unrelated_tags_are_ignored(self):
        for tag in ("v99.0.0-rc.1", "v99.0.0-alpha.2", "migrate", "not-a-version"):
            self.git("tag", tag)
        self.generate()
        self.assertEqual([entry["key"] for entry in self.manifest()["versions"]], ["v1.0", "main"])

    def test_no_fetch_uses_cached_refs_and_main_ref_only_overrides_main(self):
        cached_main = self.git("rev-parse", "main")
        cached_release = self.git("rev-parse", "release-1.0")
        for destination, source in (("heads/main", "main"),
                                    ("heads/release-1.0", "release-1.0"),
                                    ("tags/v1.0.0", "v1.0.0")):
            self.git("update-ref", "refs/docs-snapshots/" + destination, source)
        self.add_release("9.0")  # Unfetched local tags must not affect the cached release set.
        self.write_site("LOCAL PREVIEW CONTENT", "v99.0.0")
        self.commit("Unfetched local main changes")
        local_main = self.git("rev-parse", "HEAD")
        self.assertEqual(self.git("remote"), "")
        self.generate()
        versions = self.manifest()["versions"]
        self.assertEqual([entry["key"] for entry in versions], ["v1.0", "main"])
        self.assertEqual([entry["commit"] for entry in versions], [cached_release, cached_main])
        self.assertIn("MAIN CONTENT", self.page("main").read_text())
        self.generate("--main-ref", "HEAD")
        versions = self.manifest()["versions"]
        self.assertEqual([entry["key"] for entry in versions], ["v1.0", "main"])
        self.assertEqual([entry["commit"] for entry in versions], [cached_release, local_main])
        self.assertIn("LOCAL PREVIEW CONTENT", self.page("main").read_text())
        self.assertIn("BRANCH CONTENT 1.0", self.page("v1.0").read_text())

    def test_missing_cached_branch_is_not_replaced_with_local_branch(self):
        self.generate()
        previous = self.file_bytes(self.output)
        self.git("update-ref", "refs/docs-snapshots/heads/main", "main")
        self.git("update-ref", "refs/docs-snapshots/tags/v1.0.0", "v1.0.0")
        result = self.generate(success=False)
        self.assertIn("release-1.0", result.stdout + result.stderr)
        self.assertEqual(self.file_bytes(self.output), previous)

    def test_yaml_front_matter_can_end_without_a_final_newline(self):
        self.git("switch", "-q", "release-1.0")
        self.write("site/content/en/docs/empty/_index.md", "---\ntitle: Empty page\n---")
        self.commit("Front matter at end of file")
        self.git("switch", "-q", "main")
        self.generate()
        self.assertIn("title: Empty page", self.page("v1.0", "empty").read_text())

    def test_minimum_version_is_numeric_and_can_include_older_series(self):
        for series in ("0.9", "0.10", "0.11"):
            self.add_release(series)
        self.generate()
        self.assertEqual([entry["key"] for entry in self.manifest()["versions"]], ["v1.0", "main"])
        self.generate("--min-version", "0.10")
        self.assertEqual([entry["key"] for entry in self.manifest()["versions"]], ["v1.0", "main", "v0.11", "v0.10"])

    def test_missing_release_branch_preserves_previous_output(self):
        self.generate()
        previous = self.file_bytes(self.output)
        self.git("tag", "v9.0.0")
        result = self.generate(success=False)
        self.assertIn("release-9.0", result.stdout + result.stderr)
        self.assertEqual(self.file_bytes(self.output), previous)

    def test_unrecognized_output_directory_is_not_replaced(self):
        self.write("site/_generated/keep.txt", "User-owned output.\n")
        before = self.file_bytes(self.output)
        self.generate(success=False)
        self.assertEqual(self.file_bytes(self.output), before)

    def test_symlink_output_is_not_followed_or_replaced(self):
        protected = self.repo / "protected"
        protected.mkdir()
        (protected / "keep.txt").write_text("Do not remove me.\n")
        self.output.symlink_to(protected, target_is_directory=True)
        self.generate(success=False)
        self.assertTrue(self.output.is_symlink())
        self.assertEqual((protected / "keep.txt").read_text(), "Do not remove me.\n")

    def test_release_archive_symlink_preserves_previous_output(self):
        self.generate()
        previous = self.file_bytes(self.output)
        self.git("switch", "-q", "release-1.0")
        (self.repo / "site/static/escape").symlink_to(self.repo / "unrelated.txt")
        self.commit("Unsafe release asset symlink")
        self.git("switch", "-q", "main")
        result = self.generate(success=False)
        self.assertIn("Unsafe", result.stdout + result.stderr)
        self.assertEqual(self.file_bytes(self.output), previous)

    def test_current_shell_does_not_publish_external_symlink_contents(self):
        secret_folder = tempfile.TemporaryDirectory(prefix="lws-docs-private-")
        self.addCleanup(secret_folder.cleanup)
        secret = Path(secret_folder.name) / "secret.txt"
        secret.write_text("Private file outside the repository.\n")
        (self.repo / "site/static/external.txt").symlink_to(secret)
        self.generate(success=False)
        self.assertFalse(self.output.exists())
        self.assertEqual(secret.read_text(), "Private file outside the repository.\n")

    def test_regeneration_is_idempotent_and_source_files_are_untouched(self):
        before = self.file_bytes(self.repo, (".git", "site/_generated"))
        self.assertEqual(self.git("remote"), "")  # No remotes: this must work offline.
        self.generate()
        first = self.file_bytes(self.output)
        self.generate()
        self.assertEqual(self.file_bytes(self.output), first)
        self.assertEqual(self.file_bytes(self.repo, (".git", "site/_generated")), before)
        self.assertEqual(self.git("status", "--porcelain"), "")

    def test_aliases_are_namespaced_and_default_paths_redirect_to_latest(self):
        self.generate()
        for version in ("v1.0", "main"):
            page = self.page(version).read_text()
            self.assertIn("/" + version + "/docs/old-intro/", page)
            self.assertIn("/" + version + "/legacy/", page)
        self.assert_default_redirect("", "v1.0")
        self.assert_default_redirect("intro/", "v1.0")

    def test_existing_unversioned_aliases_still_reach_the_latest_page(self):
        self.generate()
        for alias in ("docs/old-intro", "legacy"):
            path = self.output / "static" / alias / "index.html"
            self.assertTrue(path.is_file(), "Missing default redirect for /" + alias + "/")
            html = path.read_text()
            self.assertTrue("/v1.0/" + alias + "/" in html or "/v1.0/docs/intro/" in html, html)

    def test_navbar_falls_back_to_docs_root_without_an_overview_page(self):
        self.generate()
        config = (self.output / "hugo.toml").read_text()
        self.assertIn('url = "/v1.0/docs/"', config)
        self.assertNotIn('url = "/v1.0/docs/overview/"', config)

    def test_default_routes_follow_hugo_directory_slugging(self):
        self.git("switch", "-q", "release-1.0")
        self.write("site/content/en/docs/Contribution Guidelines/_index.md",
                   "---\ntitle: Contribution guidelines\n---\nContribute here.\n")
        self.commit("Documentation path with spaces and capitals")
        self.git("switch", "-q", "main")
        self.generate()
        redirect = self.output / "static/docs/contribution-guidelines/index.html"
        self.assertTrue(redirect.is_file(), "Default redirects must match Hugo's slugged routes")
        self.assertIn("/v1.0/docs/contribution-guidelines/", redirect.read_text())

    def test_links_and_assets_stay_with_their_version(self):
        self.generate()
        for version, marker, branch in (("v1.0", "BRANCH CONTENT 1.0", "release-1.0"),
                                        ("main", "MAIN CONTENT", "main")):
            page = self.page(version).read_text()
            for asset in ("images/diagram.svg", "images/poster.png", "videos/demo.mp4", "examples/demo.yaml"):
                self.assertIn("/" + version + "/" + asset, page)
                self.assertEqual((self.output / "static" / version / asset).read_text(), marker + "\n")
            self.assertIn('{{< include "' + version + '/examples/demo.yaml" "yaml" >}}', page)
            self.assertIn('{{< include file="' + version + '/examples/demo.yaml" lang="yaml" >}}', page)
            self.assertIn("/" + version + "/docs/guide/?view=full#details", page)
            self.assertIn("[Absolute guide](/" + version + "/docs/guide/#details)", page)
            self.assertIn("https://example.org/docs/guide/", page)
            self.assertIn("(../guide/#details)", page)
            self.assertIn("https://raw.githubusercontent.com/kubernetes-sigs/lws/" + self.git("rev-parse", branch) + "/docs/examples/demo.yaml", page)


if __name__ == "__main__":
    unittest.main()
