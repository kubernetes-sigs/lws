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

"""Offline CLI checks: event filtering and production Netlify hook requests."""

from contextlib import redirect_stderr, redirect_stdout
import io
import json
import os
from pathlib import Path
import runpy
import sys
import tempfile
import unittest
from unittest.mock import patch
from urllib.error import HTTPError, URLError
from urllib.parse import parse_qs, urlsplit


SCRIPT = Path(__file__).with_name("trigger-docs-build.py")
HOOK_ENV = "LWS_DOCS_TEST_HOOK"
HOOK_URL = "https://api.netlify.com/build_hooks/fixture-only-secret"


class DocsBuildTriggerTest(unittest.TestCase):
    def invoke(self, event_name, event, hook=HOOK_URL, error=None):
        output = io.StringIO()
        env = dict(os.environ)
        env.pop(HOOK_ENV, None)
        if hook is not None:
            env[HOOK_ENV] = hook
        with tempfile.TemporaryDirectory(prefix="lws-docs-event-") as temporary:
            event_file = Path(temporary) / "event.json"
            event_file.write_text(json.dumps(event))
            argv = [str(SCRIPT), "--event", str(event_file), "--event-name", event_name,
                    "--hook-env", HOOK_ENV]
            with patch.dict(os.environ, env, clear=True), patch.object(sys, "argv", argv), \
                    patch("urllib.request.urlopen") as opener, \
                    redirect_stdout(output), redirect_stderr(output):
                response = opener.return_value.__enter__.return_value
                response.status = 200
                response.code = 200
                response.getcode.return_value = 200
                response.read.return_value = b"{}"
                opener.side_effect = error
                code = 0
                try:
                    runpy.run_path(str(SCRIPT), run_name="__main__")
                except SystemExit as exit_error:
                    code = exit_error.code or 0
        return code, output.getvalue(), opener

    def assert_production_post(self, opener):
        opener.assert_called_once()
        request = opener.call_args.args[0]
        self.assertEqual(request.get_method(), "POST")
        url = urlsplit(request.full_url)
        self.assertEqual(url.scheme, "https")
        self.assertEqual(url.netloc, "api.netlify.com")
        self.assertEqual(url.path, "/build_hooks/fixture-only-secret")
        self.assertEqual(parse_qs(url.query).get("trigger_branch"), ["main"])

    def test_supported_branch_pushes_build_main(self):
        for branch in ("main", "release-0.10", "release-1.0"):
            with self.subTest(branch=branch):
                code, output, opener = self.invoke("push", {"ref": "refs/heads/" + branch})
                self.assertEqual(code, 0, output)
                self.assert_production_post(opener)
                self.assertNotIn("fixture-only-secret", output)

    def test_published_stable_release_builds_main(self):
        code, output, opener = self.invoke("release", {
            "action": "published", "release": {
                "tag_name": "v1.0.1", "draft": False, "prerelease": False}})
        self.assertEqual(code, 0, output)
        self.assert_production_post(opener)

    def test_manual_trigger_builds_main(self):
        code, output, opener = self.invoke("workflow_dispatch", {})
        self.assertEqual(code, 0, output)
        self.assert_production_post(opener)

    def test_hook_branch_parameter_cannot_select_a_release_branch(self):
        code, output, opener = self.invoke("push", {"ref": "refs/heads/release-1.0"},
                                           hook=HOOK_URL + "?trigger_branch=release-1.0&clear_cache=true")
        self.assertEqual(code, 0, output)
        self.assert_production_post(opener)

    def test_irrelevant_events_skip_without_a_hook(self):
        events = [
            ("push", {"ref": "refs/heads/feature-docs"}),
            ("push", {"ref": "refs/tags/v1.0.1"}),
            ("push", {"ref": "refs/heads/release-1.0", "deleted": True}),
            ("push", {"ref": "refs/heads/main", "deleted": True}),
            ("release", {"action": "created", "release": {"tag_name": "v1.0.1"}}),
            ("release", {"action": "published", "release": {"tag_name": "v1.1.0-rc.1"}}),
            ("release", {"action": "published", "release": {"tag_name": "v1.1.0", "prerelease": True}}),
            ("release", {"action": "published", "release": {"tag_name": "v1.1.0", "draft": True}}),
            ("release", {"action": "published", "release": {"tag_name": "not-a-version"}}),
        ]
        for event_name, event in events:
            with self.subTest(event_name=event_name, event=event):
                code, output, opener = self.invoke(event_name, event, hook=None)
                self.assertEqual(code, 0, output)
                opener.assert_not_called()

    def test_missing_hook_fails_for_an_eligible_event(self):
        code, output, opener = self.invoke("push", {"ref": "refs/heads/release-1.0"}, hook=None)
        self.assertNotEqual(code, 0)
        self.assertIn(HOOK_ENV, output)
        opener.assert_not_called()

    def test_invalid_hook_urls_never_make_requests(self):
        for hook in ("http://api.netlify.com/build_hooks/fixture-only-secret",
                     "https://other.example/build_hooks/fixture-only-secret",
                     "https://api.netlify.com/other/fixture-only-secret",
                     "https://user:password@api.netlify.com/build_hooks/fixture-only-secret",
                     HOOK_URL + "#fragment"):
            with self.subTest(hook=hook):
                code, output, opener = self.invoke("workflow_dispatch", {}, hook=hook)
                self.assertNotEqual(code, 0)
                self.assertNotIn("fixture-only-secret", output)
                self.assertNotIn("password", output)
                opener.assert_not_called()

    def test_request_failure_does_not_expose_hook_secret(self):
        for error in (URLError(HOOK_URL), HTTPError(HOOK_URL, 503, HOOK_URL, None, None)):
            with self.subTest(error=type(error).__name__):
                code, output, opener = self.invoke("workflow_dispatch", {}, error=error)
                self.assertNotEqual(code, 0)
                self.assertNotIn("fixture-only-secret", output)
                opener.assert_called_once()


if __name__ == "__main__":
    unittest.main()
