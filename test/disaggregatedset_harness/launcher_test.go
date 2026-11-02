// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package harness

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func shell(t *testing.T, directory string, env []string, args ...string) ([]byte, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("Bash is required for launcher checks")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", args...)
	command.Dir, command.Env = directory, append(os.Environ(), env...)
	return command.CombinedOutput()
}

func scriptFunction(t *testing.T, name string) string {
	t.Helper()
	source, err := os.ReadFile("run.sh")
	requireNoError(t, err)
	_, body, found := strings.Cut(string(source), name+"() {\n")
	if !found {
		t.Fatalf("missing launcher function %s", name)
	}
	body, _, found = strings.Cut(body, "\n}\n")
	if !found {
		t.Fatalf("unterminated launcher function %s", name)
	}
	return "set -euo pipefail\nfail() { printf '%s\\n' \"$*\" >&2; exit 1; }\nsection() { :; }\n" + name + "() {\n" + body + "\n}\n" + name + " \"$@\"\n"
}

func testExecutable(t *testing.T, directory, name, body string) {
	t.Helper()
	requireNoError(t, os.WriteFile(filepath.Join(directory, name), []byte("#!/bin/sh\n"+body), 0700))
}

func TestLauncherReadOnlySelectionSkipsRuntimeSetup(t *testing.T) {
	root := t.TempDir()
	harness := filepath.Join(root, "test", "disaggregatedset_harness")
	requireNoError(t, os.MkdirAll(harness, 0700))
	source, err := os.ReadFile("run.sh")
	requireNoError(t, err)
	requireNoError(t, os.WriteFile(filepath.Join(harness, "run.sh"), source, 0700))
	commands := filepath.Join(root, "bin")
	requireNoError(t, os.Mkdir(commands, 0700))
	testExecutable(t, commands, "go", "printf '%s\\n' \"$*\"\nexit \"${TEST_GO_EXIT:-0}\"\n")
	testExecutable(t, commands, "docker", "echo 'Docker must not run' >&2\nexit 91\n")
	env := []string{"PATH=" + commands + string(os.PathListSeparator) + os.Getenv("PATH"), "HARNESS_MAX_PODS=0"}
	for _, args := range [][]string{
		nil, {"--help"}, {"-h"}, {"-help"}, {"list"}, {"dry-run", "large-all"},
		{"--h"}, {"-h=true"}, {"--help=false"}, {"-help=anything"},
		{"--list"}, {"-list"}, {"-validate"}, {"-dry-run"}, {"large-all", "--dry-run"},
		{"--list=true"}, {"-dry-run=1"}, {"--validate=T"}, {"--list=false", "--dry-run=true"},
	} {
		output, err := shell(t, root, env, append([]string{filepath.Join(harness, "run.sh")}, args...)...)
		if err != nil || !strings.Contains(string(output), "run ./test/disaggregatedset_harness/cmd") || strings.Contains(string(output), "Docker must not run") {
			t.Fatalf("selection %q invoked runtime setup: %s, %v", args, output, err)
		}
	}
	for _, args := range [][]string{
		{"ordinary", "--list=false"}, {"ordinary", "-dry-run=0"}, {"ordinary", "--validate=F"},
		{"list", "ordinary", "--list=false"}, {"dry-run", "ordinary", "--dry-run=false"},
		{"ordinary", "--list", "--list=false"},
	} {
		output, err := shell(t, root, env, append([]string{filepath.Join(harness, "run.sh")}, args...)...)
		if err == nil || !strings.Contains(string(output), "HARNESS_MAX_PODS must be") {
			t.Fatalf("disabled read-only flag %q skipped runtime validation: %s, %v", args, output, err)
		}
	}
	output, err := shell(t, root, append(env, "TEST_GO_EXIT=2"), filepath.Join(harness, "run.sh"), "unknown")
	if err == nil || !strings.Contains(string(output), "--validate") {
		t.Fatalf("invalid selection was accepted: %s, %v", output, err)
	}
	if _, err := os.Stat(filepath.Join(harness, ".state")); !os.IsNotExist(err) {
		t.Fatal("read-only selection created runtime state")
	}
}

func TestLauncherCheckpointModesRequireTerminalOnlyWhenInteractive(t *testing.T) {
	root := t.TempDir()
	testExecutable(t, root, "go", "exit 0\n")
	for _, tool := range []string{"docker", "kubectl", "git", "jq", "tar", "kind"} {
		testExecutable(t, root, tool, "echo 'Runtime setup reached' >&2\nexit 91\n")
	}
	script, err := filepath.Abs("run.sh")
	requireNoError(t, err)
	env := []string{"PATH=" + root + string(os.PathListSeparator) + os.Getenv("PATH"), "KIND=" + filepath.Join(root, "kind"), "HARNESS_MAX_PODS=160"}
	for _, tc := range []struct {
		flags       []string
		interactive bool
	}{
		{[]string{"-interactive"}, true}, {[]string{"--interactive=true"}, true}, {[]string{"-interactive=1"}, true},
		{[]string{"--auto=false"}, true}, {[]string{"-auto=F"}, true},
		{[]string{"--auto", "-interactive"}, true}, {[]string{"--auto", "--interactive=true"}, true},
		{[]string{"--auto", "--auto=false"}, true},
		{nil, false}, {[]string{"-auto"}, false}, {[]string{"-interactive=false"}, false},
		{[]string{"--interactive", "--auto=true"}, false}, {[]string{"-auto=false", "--interactive=false"}, false},
	} {
		args := append([]string{script, "ordinary"}, tc.flags...)
		output, err := shell(t, root, env, args...)
		needsTerminal := strings.Contains(string(output), "Interactive mode needs a terminal")
		reachedRuntime := strings.Contains(string(output), "Runtime setup reached")
		if err == nil || needsTerminal != tc.interactive || reachedRuntime == tc.interactive {
			t.Fatalf("checkpoint flags %q: %s, %v", tc.flags, output, err)
		}
	}
}

func TestLauncherRejectsInvalidPodCapacityBeforeDocker(t *testing.T) {
	root := t.TempDir()
	testExecutable(t, root, "go", "exit 0\n")
	testExecutable(t, root, "docker", "echo 'Docker must not run' >&2\nexit 91\n")
	script, err := filepath.Abs("run.sh")
	requireNoError(t, err)
	for _, value := range []string{"0", "1001", "-1", "not-a-number", "16; echo unsafe"} {
		output, err := shell(t, root, []string{"PATH=" + root + string(os.PathListSeparator) + os.Getenv("PATH"), "HARNESS_MAX_PODS=" + value}, script, "large-all", "--auto")
		if err == nil || !strings.Contains(string(output), "HARNESS_MAX_PODS must be") || strings.Contains(string(output), "Docker must not run") {
			t.Fatalf("invalid capacity %q reached Docker: %s, %v", value, output, err)
		}
	}
}

func TestLauncherOwnershipPredicateUsesExactMountAndIdentity(t *testing.T) {
	root := t.TempDir()
	testExecutable(t, root, "uname", "printf '%s\\n' \"$TEST_OS\"\n")
	owner := filepath.Join(root, "run", "owner")
	for _, tc := range []struct {
		os, id, cluster, mount string
		accepted               bool
	}{
		{"Darwin", "owned-id", "owned-cluster", owner, true},
		{"Darwin", "owned-id", "owned-cluster", "/host_mnt" + owner, true},
		{"Linux", "owned-id", "owned-cluster", "/host_mnt" + owner, false},
		{"Darwin", "owned-id", "owned-cluster", owner + "-other", false},
		{"Darwin", "replacement-id", "owned-cluster", owner, false},
		{"Darwin", "owned-id", "another-cluster", owner, false},
	} {
		env := []string{"PATH=" + root + string(os.PathListSeparator) + os.Getenv("PATH"), "TEST_OS=" + tc.os,
			"NODE_ID=owned-id", "CLUSTER=owned-cluster", "RUN_DIR=" + filepath.Dir(owner)}
		output, err := shell(t, root, env, "-c", scriptFunction(t, "owns_node"), "owns-node-test", tc.id, tc.cluster, tc.mount)
		if (err == nil) != tc.accepted {
			t.Fatalf("%+v: %s, %v", tc, output, err)
		}
	}
}

func TestLauncherKindConfigurationKeepsOwnedMount(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required for launcher JSON")
	}
	root := t.TempDir()
	output, err := shell(t, root, []string{"RUN_DIR=" + root, "HARNESS_MAX_PODS=160"}, "-c", scriptFunction(t, "kind_config"))
	requireNoError(t, err)
	var config Object
	requireNoError(t, json.Unmarshal(output, &config))
	patches := arr(config["kubeadmConfigPatches"])
	if len(patches) != 1 || !strings.Contains(str(patches[0]), "maxPods: 160") {
		t.Fatalf("wrong kubelet capacity: %s", output)
	}
	nodes := objects(config["nodes"])
	if len(nodes) != 1 {
		t.Fatalf("unexpected node count: %s", output)
	}
	mounts := objects(nodes[0]["extraMounts"])
	if len(mounts) != 1 || mounts[0]["hostPath"] != filepath.Join(root, "owner") || mounts[0]["readOnly"] != true {
		t.Fatalf("lost read-only ownership proof: %s", output)
	}
}

func TestReleaseBaselineRejectsSourceAndTagMismatch(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required for release validation")
	}
	root := t.TempDir()
	baselinePath, err := filepath.Abs("baseline.json")
	requireNoError(t, err)
	contents, err := os.ReadFile(baselinePath)
	requireNoError(t, err)
	var baseline Object
	requireNoError(t, json.Unmarshal(contents, &baseline))
	commit, image := str(baseline["commit"]), str(at(baseline, "release", "image"))
	testExecutable(t, root, "git", "printf '%s\\n' \"$TEST_RELEASE_COMMIT\"\n")
	testExecutable(t, root, "curl", "echo 'validation must not download' >&2\nexit 91\n")
	testExecutable(t, root, "shasum", "echo 'validation must not hash' >&2\nexit 91\n")
	for _, tc := range []struct {
		commit, image string
		accepted      bool
	}{{commit, image, true}, {strings.Repeat("0", 40), image, false}, {commit, "registry.k8s.io/lws/lws:v0.8.0", false}} {
		env := []string{"PATH=" + root + string(os.PathListSeparator) + os.Getenv("PATH"), "BASELINE_FILE=" + baselinePath,
			"REPOSITORY=" + root, "COMMIT=" + commit, "RELEASE_IMAGE=" + tc.image, "TEST_RELEASE_COMMIT=" + tc.commit}
		output, err := shell(t, root, env, "-c", scriptFunction(t, "validate_release"))
		if (err == nil) != tc.accepted {
			t.Fatalf("%+v: %s, %v", tc, output, err)
		}
	}
}

func TestPublishedReleaseVerifiesDigestWithoutManagerBuild(t *testing.T) {
	for _, tool := range []string{"jq", "shasum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " is required for release verification")
		}
	}
	root := t.TempDir()
	image := "registry.k8s.io/lws/lws@sha256:" + strings.Repeat("a", 64)
	testExecutable(t, root, "go", "echo 'Manager build must not run' >&2\nexit 91\n")
	testExecutable(t, root, "docker", `case "$*" in
  *RepoDigests*) printf '["%s"]\n' "$TEST_IMAGE_DIGEST" ;;
  *Architecture*) printf 'arm64\n' ;;
  "image inspect "*) exit 0 ;;
  *) echo "Unexpected Docker command: $*" >&2; exit 91 ;;
esac
`)
	testExecutable(t, root, "curl", `while [ "$#" -gt 0 ]; do
  if [ "$1" = --output ]; then shift; printf 'release fixture' > "$1"; exit 0; fi
  shift
done
exit 91
`)
	for _, actual := range []string{image, image + "wrong"} {
		env := []string{"PATH=" + root + string(os.PathListSeparator) + os.Getenv("PATH"), "RUN_DIR=" + root,
			"RELEASE_IMAGE=registry.k8s.io/lws/lws:v1.0.0", "IMAGE=" + image, "RELEASE_DIGEST=" + image,
			"TEST_IMAGE_DIGEST=" + actual, "ARCH=arm64", "RELEASE_MANIFESTS=https://example.invalid/not-fetched",
			fmt.Sprintf("RELEASE_MANIFEST_SHA=%x", sha256.Sum256([]byte("release fixture")))}
		output, err := shell(t, root, env, "-c", scriptFunction(t, "prepare_image"))
		if (err == nil) != (actual == image) || strings.Contains(string(output), "Manager build must not run") {
			t.Fatalf("release digest %q: %s, %v", actual, output, err)
		}
	}
}

func TestSourceBuildSupportsBothDockerBuilders(t *testing.T) {
	for _, provenance := range []string{"0", "1"} {
		root := t.TempDir()
		requireNoError(t, os.Mkdir(filepath.Join(root, "source"), 0700))
		testExecutable(t, root, "go", "exit 0\n")
		testExecutable(t, root, "docker", `case "$*" in
  "image inspect "*) exit 1 ;;
  "build --help") if [ "$TEST_PROVENANCE" = 1 ]; then printf '  --provenance string\n'; fi; exit 0 ;;
  "build "*) printf 'BUILD %s\n' "$*"; cat >/dev/null ;;
  *) echo "Unexpected Docker command: $*" >&2; exit 91 ;;
esac
`)
		commit := strings.Repeat("a", 40)
		output, err := shell(t, root, []string{"PATH=" + root + string(os.PathListSeparator) + os.Getenv("PATH"),
			"RUN_DIR=" + root, "RELEASE_IMAGE=", "COMMIT=" + commit, "ARCH=arm64", "IMAGE=local-controller:" + commit,
			"TEST_PROVENANCE=" + provenance}, "-c", scriptFunction(t, "prepare_image"))
		requireNoError(t, err)
		if strings.Contains(string(output), "--provenance=false") != (provenance == "1") ||
			!strings.Contains(string(output), "--platform linux/arm64") || !strings.Contains(string(output), "--build-arg REVISION="+commit) {
			t.Fatalf("builder provenance=%s: %s", provenance, output)
		}
	}
}

func TestObserverOverlayIsExplicitAndDoesNotMutateSource(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required to prepare observer overlays")
	}
	prepare, err := filepath.Abs(filepath.Join("observer", "prepare.sh"))
	requireNoError(t, err)
	for _, tc := range []struct {
		inputs, state bool
		tags          string
	}{
		{true, true, "harness"}, {false, false, "harness,harness_legacy_scale_up"},
		{true, false, ""}, {false, true, ""},
	} {
		root := t.TempDir()
		source := filepath.Join(root, "source with spaces")
		controller := filepath.Join(source, "pkg", "controllers", "disaggregatedset")
		requireNoError(t, os.MkdirAll(controller, 0700))
		requireNoError(t, os.WriteFile(filepath.Join(source, "go.mod"), []byte("module fixture\n"), 0600))
		inputText, stateText := "// scaleUpDuringRollout bool\n", "// ScaleUpDuringRollout bool\n"
		if tc.inputs {
			inputText += "\tscaleUpDuringRollout bool\n"
		}
		if tc.state {
			stateText += "\tScaleUpDuringRollout bool\n"
		}
		requireNoError(t, os.WriteFile(filepath.Join(controller, "executor.go"), []byte(inputText), 0600))
		requireNoError(t, os.WriteFile(filepath.Join(controller, "planner_state.go"), []byte(stateText), 0600))
		overlay := filepath.Join(root, "overlay.json")
		output, err := shell(t, root, nil, prepare, source, overlay, "--tests")
		if (err == nil) != (tc.tags != "") {
			t.Fatalf("%+v: %s, %v", tc, output, err)
		}
		if tc.tags == "" {
			if _, err := os.Stat(overlay); !os.IsNotExist(err) {
				t.Fatal("inconsistent source produced an overlay")
			}
			continue
		}
		if strings.TrimSpace(string(output)) != tc.tags {
			t.Fatalf("wrong build tags: %s", output)
		}
		contents, err := os.ReadFile(overlay)
		requireNoError(t, err)
		var mapping struct{ Replace map[string]string }
		requireNoError(t, json.Unmarshal(contents, &mapping))
		if len(mapping.Replace) != 6 {
			t.Fatalf("missing build/test overlay assets: %+v", mapping)
		}
		for virtual, backing := range mapping.Replace {
			if !strings.HasPrefix(virtual, source+string(os.PathSeparator)) || !strings.HasSuffix(backing, ".go.inc") {
				t.Fatalf("invalid overlay entry: %s -> %s", virtual, backing)
			}
			if _, err := os.Stat(virtual); !os.IsNotExist(err) {
				t.Fatalf("overlay materialized a controller hook: %s", virtual)
			}
			if _, err := os.Stat(backing); err != nil {
				t.Fatal(err)
			}
		}
		output, err = shell(t, root, nil, prepare, source, overlay)
		if err == nil || !strings.Contains(string(output), "already exists") {
			t.Fatal("existing overlay was overwritten")
		}
	}
}

func TestObserverBuildFailureDoesNotFallback(t *testing.T) {
	root := t.TempDir()
	harness, err := filepath.Abs(".")
	requireNoError(t, err)
	prepare := filepath.Join(root, "observer")
	requireNoError(t, os.Mkdir(prepare, 0700))
	testExecutable(t, prepare, "prepare.sh", "printf 'harness\\n'\n")
	testExecutable(t, root, "go", "printf '%s\\n' \"$*\" >> \"$TEST_COMMAND_LOG\"\nexit 91\n")
	requireNoError(t, os.Mkdir(filepath.Join(root, "source"), 0700))
	log := filepath.Join(root, "commands.log")
	output, err := shell(t, harness, []string{"PATH=" + root + string(os.PathListSeparator) + os.Getenv("PATH"),
		"RUN_DIR=" + root, "HARNESS_DIR=" + root, "TEST_COMMAND_LOG=" + log}, "-c", scriptFunction(t, "build_observer"))
	if err == nil {
		t.Fatalf("accepted failed build: %s", output)
	}
	contents, err := os.ReadFile(log)
	requireNoError(t, err)
	if strings.Count(strings.TrimSpace(string(contents)), "\n") != 0 || !strings.Contains(string(contents), "-tags=harness ") {
		t.Fatalf("build retried with other semantics: %s", contents)
	}
}
