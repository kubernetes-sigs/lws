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

"""Render the real version-link partials in a theme-free, offline Hugo fixture."""

from html.parser import HTMLParser
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

HUGO = shutil.which(os.environ.get("HUGO", "hugo"))
PARTIALS = Path(__file__).resolve().parents[2] / "site/layouts/partials"
MISSING = "Page unavailable → Overview"


class Links(HTMLParser):
    def __init__(self, html):
        super().__init__()
        self.links, self.current, self.banner = [], None, False
        self.feed(html)

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        classes = attrs.get("class", "").split()
        if tag == "aside" and "docs-version-banner" in classes:
            self.banner = True
        if tag == "a":
            self.current = dict(href=attrs["href"], banner=self.banner,
                                menu="dropdown-item" in classes, text="")

    def handle_data(self, data):
        if self.current is not None:
            self.current["text"] += data

    def handle_endtag(self, tag):
        if tag == "a" and self.current is not None:
            self.current["text"] = " ".join(self.current["text"].split())
            self.links.append(self.current)
            self.current = None
        if tag == "aside":
            self.banner = False


@unittest.skipUnless(HUGO, "Hugo not found; set HUGO to its executable or add it to PATH")
class DocsVersionLinksTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        temporary = tempfile.TemporaryDirectory(prefix="lws-version-links-test-")
        cls.addClassCleanup(temporary.cleanup)
        cls.site = Path(temporary.name)

        def write(relative, text):
            path = cls.site / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(text, encoding="utf-8")

        write("hugo.toml", 'baseURL = "https://docs.example/"\n')
        layout = ('{{ partial "navbar-version-selector.html" . }}'
                  '{{ partial "docs-version-banner.html" . }}{{ .Content }}')
        for kind in ("list", "single"):
            write(f"layouts/_default/{kind}.html", layout)
        (cls.site / "layouts/partials").mkdir()
        for name in ("navbar-version-selector", "docs-version-banner", "docs-version-target"):
            shutil.copyfile(PARTIALS / f"{name}.html", cls.site / f"layouts/partials/{name}.html")

        versions = [dict(key="v1.0", prefix="stable", label="v1.0.0", latest=True),
                    dict(key="v0.11", prefix="v0.11", label="v0.11.1", latest=False),
                    dict(key="main", prefix="main", label="main", latest=False)]
        for version in versions:
            prefix = version["prefix"]
            version.update(url=f"/{prefix}/docs/", commit="1234567890abcdef")
            pages = ["_index.md", "shared.md"]
            pages += (["old-only.md"] if prefix == "v0.11" else
                      ["overview/_index.md", "section/_index.md", "custom.md"])
            if prefix == "main":
                pages.append("main-only.md")
            for page in pages:
                front = dict(title=page, params=dict(docs_version=version["key"],
                                                    docs_source_prefix=prefix))
                if prefix == "stable" and page == "custom.md":
                    front["url"] = "/custom-stable-url/"
                write(f"content/{prefix}/docs/{page}", json.dumps(front) + "\n")
        write("content/unversioned.md", '{"title": "Outside docs"}\n')
        write("data/docs_versions.json", json.dumps(dict(latest="v1.0", versions=versions)))
        result = subprocess.run([HUGO, "--source", str(cls.site)], capture_output=True,
                                text=True, timeout=30)
        if result.returncode:
            raise RuntimeError(result.stdout + result.stderr)

    def links(self, route):
        return Links((self.site / "public" / route / "index.html").read_text(encoding="utf-8")).links

    def assert_stable_target(self, route, expected, missing=False):
        links = self.links(route)
        banner = next(link for link in links if link["banner"])
        menu = next(link for link in links if link["menu"] and link["text"].startswith("v1.0.0"))
        for link in (banner, menu):
            self.assertEqual(link["href"], expected, route)
            self.assertEqual(MISSING in link["text"], missing, route)

    def test_same_regular_section_and_custom_url_pages(self):
        for source, expected in (("main/docs/shared", "/stable/docs/shared/"),
                                 ("v0.11/docs/shared", "/stable/docs/shared/"),
                                 ("main/docs/section", "/stable/docs/section/"),
                                 ("main/docs/custom", "/custom-stable-url/")):
            with self.subTest(source=source):
                self.assert_stable_target(source, expected)

    def test_main_only_and_old_only_pages_fall_back_to_stable_overview(self):
        for source in ("main/docs/main-only", "v0.11/docs/old-only"):
            with self.subTest(source=source):
                self.assert_stable_target(source, "/stable/docs/overview/", missing=True)

    def test_missing_menu_pages_use_overview_or_manifest_url(self):
        for source, target, expected in (("main/docs/section", "v0.11.1", "/v0.11/docs/"),
                                         ("v0.11/docs/old-only", "main", "/main/docs/overview/")):
            with self.subTest(source=source, target=target):
                link = next(link for link in self.links(source)
                            if link["menu"] and link["text"].startswith(target))
                self.assertEqual(link["href"], expected)
                self.assertIn(MISSING, link["text"])

    def test_unversioned_page_uses_overview_without_missing_hint(self):
        links = self.links("unversioned")
        stable = next(link for link in links if link["menu"] and link["text"].startswith("v1.0.0"))
        self.assertEqual(stable["href"], "/stable/docs/overview/")
        self.assertNotIn(MISSING, stable["text"])
        self.assertFalse(any(link["banner"] for link in links))

    def test_latest_stable_has_no_warning(self):
        self.assertFalse(any(link["banner"] for link in self.links("stable/docs/shared")))


if __name__ == "__main__":
    unittest.main()
