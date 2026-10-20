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

"""Request a main-based combined-site rebuild after a release-docs change.

The hook is a secret; never print its value, even on HTTP errors. This helper is
also usable by a future Prow postsubmit instead of the GitHub Actions workflow.
"""

import argparse
import json
import os
from pathlib import Path
import re
import sys
import urllib.error
import urllib.parse
import urllib.request


def should_build(name, event):
    if name == "push":
        return not event.get("deleted", False) and bool(re.fullmatch(
            r"refs/heads/(main|release-\d+\.\d+)", event.get("ref", "")))
    if name == "release":
        release = event.get("release", {})
        return (event.get("action") == "published" and not release.get("draft", False)
                and not release.get("prerelease", False)
                and bool(re.fullmatch(r"v\d+\.\d+\.\d+", release.get("tag_name", ""))))
    return name == "workflow_dispatch"


def request_build(hook):
    url = urllib.parse.urlsplit(hook)
    if (url.scheme != "https" or url.netloc != "api.netlify.com"
            or not re.fullmatch(r"/build_hooks/[A-Za-z0-9_-]+", url.path)
            or url.fragment):
        raise ValueError("Build hook must be an HTTPS api.netlify.com/build_hooks URL")
    query = dict(urllib.parse.parse_qsl(url.query))
    # A release merge changes one source tree, but must NOT deploy the old
    # website configuration from that branch over the combined production site.
    query["trigger_branch"] = "main"
    target = urllib.parse.urlunsplit(url._replace(query=urllib.parse.urlencode(query)))
    request = urllib.request.Request(target, data=b"{}", method="POST",
                                     headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=30) as response:
        if not 200 <= response.status < 300:
            raise ValueError("Netlify did not accept the build request")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--event", type=Path, required=True)
    parser.add_argument("--event-name", required=True)
    parser.add_argument("--hook-env", default="NETLIFY_DOCS_BUILD_HOOK")
    args = parser.parse_args()
    try:
        event = json.loads(args.event.read_text())
        if not should_build(args.event_name, event):
            print("No documentation rebuild needed for this event.")
            return 0
        hook = os.environ.get(args.hook_env, "")
        if not hook:
            raise ValueError(f"{args.hook_env} is not configured; see site/README.md")
        request_build(hook)
    except urllib.error.HTTPError as error:
        print(f"Documentation rebuild request failed (HTTP {error.code}).", file=sys.stderr)
        return 1
    except (OSError, urllib.error.URLError, json.JSONDecodeError):
        print("Could not read the event or contact Netlify; no secret values are logged.", file=sys.stderr)
        return 1
    except ValueError as error:
        print(str(error), file=sys.stderr)
        return 1
    print("Requested a combined documentation build from main.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
