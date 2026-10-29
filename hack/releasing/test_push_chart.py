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

"""Offline publishing checks: real Helm packages, fake network and uploads."""

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
VERSION = "0.0.0-dev-" + SHA
IMAGE_REPO = "images.example.invalid/lws/lws"
CHART_REPO = "us-central1-docker.pkg.dev/test-project/lws/charts"
PACKAGE_NAME = "projects/test-project/locations/us-central1/repositories/lws/packages/charts%2Flws"
DIGEST = "sha256:" + "a" * 64
PREVIOUS_DIGEST = "sha256:" + "b" * 64
CHART_DIGEST = "sha256:" + "c" * 64
LAYER_DIGEST = "sha256:" + "d" * 64

# This wrapper refuses unrecognized commands, including real network requests.
WRAPPER = '''import json, os, pathlib, shutil, sys
tool, args = pathlib.Path(sys.argv[0]).name, sys.argv[1:]
env = os.environ
scenario = env["SCENARIO"]
with open(env["CALL_LOG"], "a") as log:
    log.write(json.dumps([tool, args]) + "\\n")
def flag(name):
    return args[args.index(name) + 1]
if tool == "helm":
    assert args[0] in ("package", "push", "show"), args
    if scenario == args[0] + "-failure":
        sys.exit(7)
    if args[0] != "push":
        os.execv(env["HELM_REAL"], [env["HELM_REAL"], *args])
    assert pathlib.Path(args[1]).is_file(), args
elif tool == "gcloud":
    assert args == ["auth", "print-access-token"], args
    if scenario == "auth-failure":
        sys.exit(7)
    print("offline-test-access-token")
elif tool == "curl":
    url = args[-1]
    if url.startswith("https://api.github.com/repos/kubernetes-sigs/lws/commits/"):
        assert url.endswith(env["BUILD_SHA"]), args
        if scenario == "lookup-failure":
            sys.exit(7)
        print(json.dumps({"sha": env["RESOLVED_SHA"]}))
        sys.exit(0)
    assert url.startswith("https://artifactregistry.googleapis.com/v1/" + env["PACKAGE_NAME"] + "/tags"), args
    auth = pathlib.Path(flag("--config"))
    assert auth.stat().st_mode & 0o777 == 0o600
    assert auth.read_text() == 'header = "Authorization: Bearer offline-test-access-token"\\n'
    if scenario == "network-failure":
        sys.exit(7)
    response = pathlib.Path(flag("--output"))
    if "--request" in args:
        assert flag("--request") == "POST", args
        assert url.endswith("?tagId=" + env["CHART_VERSION"]), args
        body = json.loads(pathlib.Path(flag("--data-binary")[1:]).read_text())
        assert body == {"version": env["PACKAGE_NAME"] + "/versions/" + env["CHART_DIGEST"]}, body
        status = 409 if scenario == "concurrent" else 500 if scenario == "create-failure" else 200
        payload = {"error": {"status": "ALREADY_EXISTS"}} if status == 409 else body
        if scenario == "create-wrong-version":
            payload = {"version": "another/version"}
        if scenario == "create-conflict-unverified":
            status, payload = 409, {"error": {"status": "ABORTED"}}
    else:
        assert url.endswith("/tags/" + env["CHART_VERSION"]), args
        counter = pathlib.Path(env["GET_COUNT"])
        count = int(counter.read_text()) + 1 if counter.exists() else 1
        counter.write_text(str(count))
        exists = scenario.startswith("existing") or (scenario == "concurrent" and count > 1)
        status = 200 if exists else 404
        if scenario in ("unauthorized", "forbidden", "registry-failure"):
            status = {"unauthorized": 401, "forbidden": 403, "registry-failure": 500}[scenario]
        payload = {"version": env["PACKAGE_NAME"] + "/versions/" + env["CHART_DIGEST"]} if exists else {"error": {"status": "NOT_FOUND"}}
        if scenario == "ambiguous-not-found":
            payload = {"error": {"status": "PERMISSION_DENIED"}}
    response.write_text(json.dumps(payload))
    print(status, end="")
elif tool == "oras":
    if args[0] == "login":
        assert sys.stdin.read() == "offline-test-access-token"
        pathlib.Path(flag("--registry-config")).write_text("{}")
    elif args[0] == "push":
        if scenario == "upload-failure":
            sys.exit(7)
        assert flag("--image-spec") == "v1.0", args
        assert flag("--format") == "json", args
        assert env["CHART_REPO"] + "/lws" in args, args
        assert not any(arg.startswith(env["CHART_REPO"] + "/lws:") for arg in args), args
        assert flag("--config").endswith(":application/vnd.cncf.helm.config.v1+json"), args
        package = pathlib.Path(args[-1].removesuffix(":application/vnd.cncf.helm.chart.content.v1.tar+gzip"))
        assert package.is_file(), args
        print(json.dumps({"digest": env["CHART_DIGEST"]}))
    elif args[:2] == ["manifest", "fetch"]:
        assert args[-1] == env["CHART_REPO"] + "/lws@" + env["CHART_DIGEST"], args
        print(json.dumps({"config": {"mediaType": "application/vnd.cncf.helm.config.v1+json"},
                          "layers": [{"mediaType": "application/vnd.cncf.helm.chart.content.v1.tar+gzip", "digest": env["LAYER_DIGEST"]}]}))
    elif args[:2] == ["blob", "fetch"]:
        assert args[-1] == env["CHART_REPO"] + "/lws@" + env["LAYER_DIGEST"], args
        shutil.copy2(env["EXISTING_PACKAGE"], flag("--output"))
    else:
        sys.exit(8)
else:
    sys.exit(8)
'''


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

    def publish(self, extra_tag="main", scenario="new", build=BUILD, resolved_sha=SHA,
                image_digest=DIGEST):
        temporary = tempfile.TemporaryDirectory(prefix="lws-push-chart-test-")
        self.addCleanup(temporary.cleanup)
        repo = Path(temporary.name)
        shutil.copytree(ROOT / "charts/lws", repo / "charts/lws")
        shutil.copy2(ROOT / "hack/push-chart.sh", repo / "push-chart.sh")
        values = repo / "charts/lws/values.yaml"
        subprocess.run([self.yq, "-i", '.image.manager.tag = "local-only" | '
                        '.image.manager.pullPolicy = "Never"', str(values)], check=True)
        before = values.read_bytes()
        chart_before = (repo / "charts/lws/Chart.yaml").read_bytes()

        existing = repo / "existing/lws"
        shutil.copytree(repo / "charts/lws", existing)
        prior_sha = "f" * 40 if scenario == "existing-wrong-source" else SHA
        prior_digest = "invalid" if scenario == "existing-unpinned" else PREVIOUS_DIGEST
        prior_repo = "other/image" if scenario == "existing-wrong-repository" else IMAGE_REPO
        prior_tag = "v20261028-gffffffff" if scenario == "existing-wrong-image-source" else BUILD
        prior_app = "v20261029-g" + SHA[:8] if scenario == "existing-wrong-app-version" else prior_tag
        subprocess.run([self.yq, "-i", f'.annotations."org.opencontainers.image.revision" = "{prior_sha}"',
                        str(existing / "Chart.yaml")], check=True)
        subprocess.run([self.yq, "-i", f'.image.manager.repository = "{prior_repo}" | '
                        f'.image.manager.digest = "{prior_digest}" | .image.manager.tag = "{prior_tag}"',
                        str(existing / "values.yaml")], check=True)
        subprocess.run([self.helm, "package", "--version", VERSION, "--app-version", prior_app,
                        str(existing), "-d", str(repo / "existing")], check=True, capture_output=True)

        wrappers = repo / "fake tools"
        wrappers.mkdir()
        for name in ("helm", "gcloud", "curl", "oras", "git"):
            path = wrappers / name
            path.write_text("#!" + sys.executable + "\n" + WRAPPER)
            path.chmod(0o755)
        log = repo / "calls.jsonl"
        destination = repo / "output packages"
        metadata = repo / "build-metadata.json"
        metadata.write_text(json.dumps({"containerimage.digest": image_digest}))
        env = dict(os.environ, PATH=str(wrappers) + os.pathsep + os.environ["PATH"],
                   HELM=str(wrappers / "helm"), HELM_REAL=self.helm, YQ=self.yq,
                   ORAS=str(wrappers / "oras"),
                   DEST_CHART_DIR=str(destination), EXTRA_TAG=extra_tag, GIT_TAG=build,
                   IMAGE_REPO=IMAGE_REPO, HELM_CHART_REPO=CHART_REPO, CHART_REPO=CHART_REPO,
                   IMAGE_BUILD_METADATA=str(metadata), SCENARIO=scenario, CALL_LOG=str(log),
                   RESOLVED_SHA=resolved_sha, BUILD_SHA=build.rsplit("-g", 1)[-1],
                   CHART_VERSION=VERSION, PACKAGE_NAME=PACKAGE_NAME,
                   CHART_DIGEST=CHART_DIGEST, LAYER_DIGEST=LAYER_DIGEST,
                   EXISTING_PACKAGE=str(repo / "existing" / f"lws-{VERSION}.tgz"),
                   GET_COUNT=str(repo / "get-count"))
        result = subprocess.run(["bash", "push-chart.sh"], cwd=repo, env=env,
                                capture_output=True, text=True, timeout=30)
        self.assertEqual(values.read_bytes(), before, "Original chart values changed")
        self.assertEqual((repo / "charts/lws/Chart.yaml").read_bytes(), chart_before)
        self.assertNotIn("offline-test-access-token", result.stdout + result.stderr)
        calls = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
        packages = list(destination.glob("*.tgz")) if destination.exists() else []
        return result, packages, calls

    def package(self, path):
        with tarfile.open(path) as archive:
            def yaml(name):
                raw = archive.extractfile("lws/" + name).read()
                result = subprocess.run([self.yq, "-o=json", ".", "-"], input=raw,
                                        capture_output=True, check=True)
                return json.loads(result.stdout)
            return yaml("Chart.yaml"), yaml("values.yaml")["image"]["manager"]

    def test_main_publishes_one_commit_chart_pinned_to_build_digest(self):
        result, packages, calls = self.publish()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(len(packages), 1)
        chart, image = self.package(packages[0])
        self.assertEqual(chart["version"], VERSION)
        self.assertEqual(chart["appVersion"], BUILD)
        self.assertEqual(chart["annotations"]["org.opencontainers.image.revision"], SHA)
        self.assertEqual(image["repository"], IMAGE_REPO)
        self.assertEqual(image["tag"], BUILD)
        self.assertEqual(image["digest"], DIGEST)
        self.assertEqual(image["pullPolicy"], "IfNotPresent")
        rendered = subprocess.run([self.helm, "template", "lws", str(packages[0])],
                                  check=True, capture_output=True, text=True).stdout
        self.assertIn(IMAGE_REPO + "@" + DIGEST, rendered)
        self.assertFalse(any(tool == "helm" and args[0] == "push" for tool, args in calls))
        self.assertEqual(sum(tool == "oras" and args[0] == "push" for tool, args in calls), 1)
        self.assertEqual(sum(tool == "curl" and "POST" in args for tool, args in calls), 1)

    def test_retries_and_concurrent_builds_reuse_the_first_pinned_image(self):
        for scenario in ("existing", "concurrent"):
            with self.subTest(scenario=scenario):
                result, packages, calls = self.publish(scenario=scenario)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("Reusing " + VERSION, result.stdout)
                self.assertIn(PREVIOUS_DIGEST, result.stdout)
                self.assertEqual(len(packages), int(scenario == "concurrent"))
                self.assertEqual(sum(tool == "curl" and "POST" in args for tool, args in calls),
                                 int(scenario == "concurrent"))
                self.assertFalse(any(tool == "helm" and args[0] == "push" for tool, args in calls))

    def test_failures_do_not_publish_or_overwrite_a_tag(self):
        for scenario in ("lookup-failure", "auth-failure", "network-failure", "unauthorized", "forbidden",
                         "registry-failure", "ambiguous-not-found", "package-failure", "upload-failure",
                         "create-failure", "create-wrong-version", "create-conflict-unverified", "existing-wrong-source", "existing-unpinned",
                         "existing-wrong-repository", "existing-wrong-image-source", "existing-wrong-app-version"):
            with self.subTest(scenario=scenario):
                result, _, calls = self.publish(scenario=scenario)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                creates = sum(tool == "curl" and "POST" in args for tool, args in calls)
                self.assertEqual(creates, int(scenario.startswith("create-")))
                self.assertFalse(any(tool == "helm" and args[0] == "push" for tool, args in calls))
                self.assertFalse(any(tool == "curl" and ("PATCH" in args or "PUT" in args)
                                     for tool, args in calls))

    def test_invalid_source_or_digest_fails_before_registry_access(self):
        for options in ({"build": "1.1.0"}, {"build": BUILD + "-dirty"},
                        {"resolved_sha": SHA[:8]}, {"resolved_sha": "f" * 40},
                        {"image_digest": ""}, {"image_digest": "sha256:bad"}):
            with self.subTest(options=options):
                result, packages, calls = self.publish(**options)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertFalse(packages)
                self.assertFalse(any(tool in ("gcloud", "oras") for tool, _ in calls))

    def test_release_paths_keep_existing_chart_and_image_versions(self):
        for ref, version, repository in (("release-1.1", BUILD, IMAGE_REPO),
                                         ("v1.1.0", "v1.1.0", "registry.k8s.io/lws/lws")):
            with self.subTest(ref=ref):
                result, packages, calls = self.publish(extra_tag=ref)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(len(packages), 1)
                chart, image = self.package(packages[0])
                self.assertEqual(chart["version"], version)
                self.assertEqual(image["tag"], version)
                self.assertEqual(image["repository"], repository)
                self.assertEqual(image["digest"], "")
                self.assertTrue(all(tool == "helm" for tool, _ in calls))
                self.assertEqual(sum(args[0] == "push" for _, args in calls), 1)


if __name__ == "__main__":
    unittest.main()
