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

"""Offline publishing checks: real Helm packages, fake pushes and Git lookups."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SHA = "1234567890abcdef1234567890abcdef12345678"
BUILD = "v20261028-g" + SHA[:8]
IMAGE_REPO = "images.example.invalid/lws/lws"
CHART_REPO = "charts.example.invalid/lws"


class PushChartTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.helm = cls.tool("HELM_REAL", "helm", os.environ.get("HELM"))
        cls.yq = cls.tool("YQ", "yq")

    @staticmethod
    def tool(variable, name, fallback=None):
        path = os.environ.get(variable) or fallback or shutil.which(name) or ROOT / "bin" / name
        resolved = shutil.which(str(path))
        if not resolved:
            raise RuntimeError(f"{name} is required; set {variable} to its executable path")
        return str(Path(resolved).resolve())

    def publish(self, extra_tag="main", head=SHA, fail="", build=BUILD):
        temporary = tempfile.TemporaryDirectory(prefix="lws-push-chart-test-")
        self.addCleanup(temporary.cleanup)
        repo = Path(temporary.name)
        shutil.copytree(ROOT / "charts/lws", repo / "charts/lws")
        shutil.copy2(ROOT / "hack/push-chart.sh", repo / "push-chart.sh")
        values = repo / "charts/lws/values.yaml"
        subprocess.run([self.yq, "-i", '.image.manager.tag = "local-only" | '
                        '.image.manager.pullPolicy = "Never"', str(values)], check=True)
        before = values.read_bytes()
        wrappers = repo / "fake tools"
        wrappers.mkdir()
        # Only packaging reaches a real tool. Any unexpected command fails closed.
        wrapper = "#!" + sys.executable + "\n" + '''import json, os, pathlib, sys
tool, args = pathlib.Path(sys.argv[0]).name, sys.argv[1:]
with open(os.environ["CALL_LOG"], "a") as log:
    log.write(json.dumps([tool, args]) + "\\n")
if tool == "helm":
    assert args[0] in ("package", "push"), args
    if os.environ["FAIL_COMMAND"] == args[0]:
        sys.exit(7)
    if args[0] == "package":
        os.execv(os.environ["HELM_REAL"], [os.environ["HELM_REAL"], *args])
    assert pathlib.Path(args[1]).is_file(), args
elif tool == "git":
    assert args == ["ls-remote", "https://github.com/kubernetes-sigs/lws.git", "refs/heads/main"], args
    if os.environ["FAIL_COMMAND"] == "lookup":
        sys.exit(7)
    if os.environ["MAIN_HEAD"]:
        print(os.environ["MAIN_HEAD"] + "\\trefs/heads/main")
else:
    sys.exit(8)
'''
        for name in ("helm", "git"):
            path = wrappers / name
            path.write_text(wrapper)
            path.chmod(0o755)
        log = repo / "calls.jsonl"
        destination = repo / "output packages"
        env = dict(os.environ, PATH=str(wrappers) + os.pathsep + os.environ["PATH"],
                   HELM=str(wrappers / "helm"), HELM_REAL=self.helm, YQ=self.yq,
                   DEST_CHART_DIR=str(destination), EXTRA_TAG=extra_tag, GIT_TAG=build,
                   IMAGE_REPO=IMAGE_REPO, HELM_CHART_REPO=CHART_REPO,
                   MAIN_HEAD=head, FAIL_COMMAND=fail, CALL_LOG=str(log))
        result = subprocess.run(["bash", "push-chart.sh"], cwd=repo, env=env,
                                capture_output=True, text=True, timeout=30)
        self.assertEqual(values.read_bytes(), before, "Original chart values changed")
        calls = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
        pushes = [args for tool, args in calls if tool == "helm" and args[0] == "push"]
        for args in pushes:
            self.assertEqual(Path(args[1]).parent, destination)
            self.assertEqual(args[2], "oci://" + CHART_REPO)
        return result, pushes, calls

    def package(self, push):
        with tarfile.open(push[1]) as archive:
            def yaml(name):
                raw = archive.extractfile("lws/" + name).read()
                result = subprocess.run([self.yq, "-o=json", ".", "-"], input=raw,
                                        capture_output=True, check=True)
                return json.loads(result.stdout)
            return yaml("Chart.yaml"), yaml("values.yaml")["image"]["manager"]

    def test_main_publishes_matching_build_and_rolling_charts(self):
        result, pushes, _ = self.publish()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(len(pushes), 2)
        for push, version in zip(pushes, (BUILD, "0.0.0-main")):
            chart, image = self.package(push)
            self.assertEqual(chart["version"], version)
            self.assertEqual(chart["appVersion"], BUILD)
            self.assertEqual(image["repository"], IMAGE_REPO)
            self.assertEqual(image["tag"], BUILD)
            self.assertEqual(image["pullPolicy"], "IfNotPresent")

    def test_release_and_superseded_builds_do_not_publish_rolling_chart(self):
        for ref, head, version, image_repo in (
                ("release-1.1", SHA, BUILD, IMAGE_REPO),
                ("v1.1.0", SHA, "v1.1.0", "registry.k8s.io/lws/lws"),
                ("main", "f" * 40, BUILD, IMAGE_REPO)):
            with self.subTest(ref=ref):
                result, pushes, calls = self.publish(extra_tag=ref, head=head)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(len(pushes), 1)
                chart, image = self.package(pushes[0])
                self.assertEqual(chart["version"], version)
                self.assertEqual(image["tag"], version)
                self.assertEqual(image["repository"], image_repo)
                self.assertEqual(any(tool == "git" for tool, _ in calls), ref == "main")

    def test_failures_preserve_values_and_never_push_rolling_chart(self):
        for fail, head, build in (
                ("package", SHA, BUILD), ("push", SHA, BUILD),
                ("lookup", SHA, BUILD), ("", "", BUILD),
                ("", "not-a-commit", BUILD), ("", SHA, "1.1.0")):
            with self.subTest(fail=fail, head=head, build=build):
                result, pushes, _ = self.publish(fail=fail, head=head, build=build)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertTrue(all("0.0.0-main" not in push[1] for push in pushes))


if __name__ == "__main__":
    unittest.main()
