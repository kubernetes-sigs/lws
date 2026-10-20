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

"""Offline checks for build event selection and production hook requests."""

from contextlib import redirect_stderr
import io
import os
from pathlib import Path
import runpy
import sys
import tempfile
import unittest
from unittest.mock import patch
from urllib.error import URLError
from urllib.parse import parse_qs, urlsplit

SCRIPT = Path(__file__).with_name("trigger-docs-build.py")
TRIGGER = runpy.run_path(str(SCRIPT))
HOOK = "https://api.netlify.com/build_hooks/fixture-only-secret"


class DocsBuildTriggerTest(unittest.TestCase):
    def test_event_filtering(self):
        cases = [
            ("push", {"ref": "refs/heads/main"}, True),
            ("push", {"ref": "refs/heads/release-1.0"}, True),
            ("push", {"ref": "refs/heads/feature-docs"}, False),
            ("push", {"ref": "refs/heads/release-1.0", "deleted": True}, False),
            ("release", {"action": "published", "release": {"tag_name": "v1.0.1"}}, True),
            ("release", {"action": "created", "release": {"tag_name": "v1.0.1"}}, False),
            ("release", {"action": "published", "release": {"tag_name": "v1.1.0-rc.1"}}, False),
            ("release", {"action": "published", "release": {"tag_name": "v1.0.1", "draft": True}}, False),
            ("workflow_dispatch", {}, True),
        ]
        for name, event, expected in cases:
            with self.subTest(name=name, event=event):
                self.assertEqual(TRIGGER["should_build"](name, event), expected)

    def test_hook_always_posts_to_main(self):
        with patch("urllib.request.urlopen") as opener:
            opener.return_value.__enter__.return_value.status = 202
            TRIGGER["request_build"](HOOK + "?trigger_branch=release-1.0")
        opener.assert_called_once()
        request = opener.call_args.args[0]
        self.assertEqual(request.get_method(), "POST")
        self.assertEqual(urlsplit(request.full_url)._replace(query="").geturl(), HOOK)
        self.assertEqual(parse_qs(urlsplit(request.full_url).query)["trigger_branch"], ["main"])

    def test_failed_request_does_not_log_the_secret(self):
        with tempfile.TemporaryDirectory(prefix="lws-docs-event-") as temporary:
            event = Path(temporary) / "event.json"
            event.write_text("{}")
            argv = [str(SCRIPT), "--event", str(event), "--event-name", "workflow_dispatch"]
            output = io.StringIO()
            with patch.dict(os.environ, {"NETLIFY_DOCS_BUILD_HOOK": HOOK}, clear=True), \
                    patch.object(sys, "argv", argv), redirect_stderr(output), \
                    patch("urllib.request.urlopen", side_effect=URLError(HOOK)) as opener:
                self.assertEqual(TRIGGER["main"](), 1)
            opener.assert_called_once()
            self.assertNotIn("fixture-only-secret", output.getvalue())


if __name__ == "__main__":
    unittest.main()
