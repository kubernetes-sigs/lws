// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package harness

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
	require.NoError(t, err)
	_, body, found := strings.Cut(string(source), name+"() {\n")
	require.True(t, found, "missing launcher function %s", name)
	body, _, found = strings.Cut(body, "\n}\n")
	require.True(t, found, "unterminated launcher function %s", name)
	return "set -euo pipefail\nfail() { printf '%s\\n' \"$*\" >&2; exit 1; }\nsection() { :; }\n" + name + "() {\n" + body + "\n}\n" + name + " \"$@\"\n"
}

func testExecutable(t *testing.T, directory, name, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(directory, name), []byte("#!/bin/sh\n"+body), 0700))
}

func TestLauncherReadOnlySelectionSkipsRuntimeSetup(t *testing.T) {
	root := t.TempDir()
	harness := filepath.Join(root, "test", "disaggregatedset_harness")
	require.NoError(t, os.MkdirAll(harness, 0700))
	source, err := os.ReadFile("run.sh")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(harness, "run.sh"), source, 0700))
	commands := filepath.Join(root, "bin")
	require.NoError(t, os.Mkdir(commands, 0700))
	testExecutable(t, commands, "go", "printf '%s\\n' \"$*\"\nexit \"${TEST_GO_EXIT:-0}\"\n")
	testExecutable(t, commands, "docker", "echo 'Docker must not run' >&2\nexit 91\n")
	env := []string{"PATH=" + commands + string(os.PathListSeparator) + os.Getenv("PATH"), "HARNESS_MAX_PODS=0"}
	for _, args := range [][]string{
		nil, {"--help"}, {"-h"}, {"-help=false"}, {"list"}, {"dry-run", "large-all"},
		{"--list"}, {"-validate"}, {"-dry-run"}, {"large-all", "--dry-run"},
		{"--list=true"}, {"-dry-run=1"}, {"--validate=T"}, {"--list=false", "--dry-run=true"},
	} {
		output, err := shell(t, root, env, append([]string{filepath.Join(harness, "run.sh")}, args...)...)
		require.NoError(t, err, "selection %q: %s", args, output)
		require.Contains(t, string(output), "run ./test/disaggregatedset_harness/cmd")
		require.NotContains(t, string(output), "Docker must not run")
	}
	for _, args := range [][]string{
		{"ordinary", "--list=false"}, {"ordinary", "-dry-run=0"}, {"ordinary", "--validate=F"},
		{"list", "ordinary", "--list=false"}, {"dry-run", "ordinary", "--dry-run=false"},
		{"ordinary", "--list", "--list=false"},
	} {
		output, err := shell(t, root, env, append([]string{filepath.Join(harness, "run.sh")}, args...)...)
		require.Error(t, err, "selection %q: %s", args, output)
		require.Contains(t, string(output), "HARNESS_MAX_PODS must be")
	}
	output, err := shell(t, root, append(env, "TEST_GO_EXIT=2"), filepath.Join(harness, "run.sh"), "unknown")
	require.Error(t, err, "%s", output)
	require.Contains(t, string(output), "--validate")
	_, err = os.Stat(filepath.Join(harness, ".state"))
	require.ErrorIs(t, err, os.ErrNotExist, "read-only selection created runtime state")
}

func TestLauncherCheckpointModesRequireTerminalOnlyWhenInteractive(t *testing.T) {
	root := t.TempDir()
	testExecutable(t, root, "go", "exit 0\n")
	for _, tool := range []string{"docker", "kubectl", "git", "jq", "tar", "kind"} {
		testExecutable(t, root, tool, "echo 'Runtime setup reached' >&2\nexit 91\n")
	}
	script, err := filepath.Abs("run.sh")
	require.NoError(t, err)
	env := []string{
		"PATH=" + root + string(os.PathListSeparator) + os.Getenv("PATH"),
		"KIND=" + filepath.Join(root, "kind"),
		"HARNESS_MAX_PODS=160",
	}
	for _, tc := range []struct {
		flags       []string
		interactive bool
	}{
		{[]string{"-interactive"}, true}, {[]string{"-interactive=1"}, true},
		{[]string{"--auto=false"}, true}, {[]string{"-auto=F"}, true},
		{[]string{"--auto", "--interactive=true"}, true},
		{[]string{"--auto", "--auto=false"}, true},
		{nil, false}, {[]string{"-auto"}, false},
		{[]string{"--interactive", "--auto=true"}, false}, {[]string{"-auto=false", "--interactive=false"}, false},
	} {
		args := append([]string{script, "ordinary"}, tc.flags...)
		output, err := shell(t, root, env, args...)
		require.Error(t, err, "flags %q: %s", tc.flags, output)
		require.Equal(t, tc.interactive, strings.Contains(string(output), "Interactive mode needs a terminal"), "%q: %s", tc.flags, output)
		require.Equal(t, !tc.interactive, strings.Contains(string(output), "Runtime setup reached"), "%q: %s", tc.flags, output)
	}
}

func TestLauncherRejectsInvalidPodCapacityBeforeDocker(t *testing.T) {
	root := t.TempDir()
	testExecutable(t, root, "go", "exit 0\n")
	testExecutable(t, root, "docker", "echo 'Docker must not run' >&2\nexit 91\n")
	script, err := filepath.Abs("run.sh")
	require.NoError(t, err)
	for _, value := range []string{"0", "1001", "-1", "not-a-number", "16; echo unsafe"} {
		output, err := shell(t, root, []string{"PATH=" + root + string(os.PathListSeparator) + os.Getenv("PATH"), "HARNESS_MAX_PODS=" + value}, script, "large-all", "--auto")
		require.Error(t, err, "capacity %q: %s", value, output)
		require.Contains(t, string(output), "HARNESS_MAX_PODS must be")
		require.NotContains(t, string(output), "Docker must not run")
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
		require.Equal(t, tc.accepted, err == nil, "%+v: %s, %v", tc, output, err)
	}
}

func TestLauncherCleanupRequiresConfirmedOwnershipOrAbsence(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required for the cleanup result")
	}
	for _, tc := range []struct {
		mode               string
		deleted, succeeded bool
	}{
		{"owned", true, true},
		{"absent", false, true},
		{"list-error", false, false},
		{"inspect-error", false, false},
		{"label-error", false, false},
		{"mount-error", false, false},
		{"replacement", false, false},
		{"extra-node", false, false},
		{"cleanup-list-error", true, false},
		{"still-present", true, false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "owner"), []byte("owned-token\n"), 0600))
			testExecutable(t, root, "docker", `printf '%s\n' "$*" >> "$RUN_DIR/docker-calls"
case "$1" in
  ps)
    case "$TEST_MODE" in
      list-error) exit 1 ;;
      absent) exit 0 ;;
      cleanup-list-error) if [ -e "$RUN_DIR/kind-called" ]; then exit 1; fi ;;
      still-present) ;;
      *) if [ -e "$RUN_DIR/kind-called" ]; then exit 0; fi ;;
    esac
    printf 'owned-id\n'
    if [ "$TEST_MODE" = extra-node ]; then printf 'foreign-id\n'; fi
    ;;
  inspect)
    if [ "$TEST_MODE" = inspect-error ]; then exit 1; fi
    case "$*" in
      *'{{.Id}}'*)
        if [ "$TEST_MODE" = replacement ]; then printf 'replacement-id\n'; else printf 'owned-id\n'; fi
        ;;
      *io.x-k8s.kind.cluster*)
        if [ "$TEST_MODE" = label-error ]; then exit 1; fi
        printf '%s\n' "$CLUSTER"
        ;;
      *Mounts*)
        if [ "$TEST_MODE" = mount-error ]; then exit 1; fi
        printf '%s\n' "$RUN_DIR/owner"
        ;;
      *) exit 91 ;;
    esac
    ;;
  *) exit 91 ;;
esac
`)
			testExecutable(t, root, "kind", `printf '%s\n' "$*" > "$RUN_DIR/kind-called"
`)
			owns := strings.TrimSuffix(scriptFunction(t, "owns_node"), "owns_node \"$@\"\n")
			env := []string{
				"PATH=" + root + string(os.PathListSeparator) + os.Getenv("PATH"),
				"TEST_MODE=" + tc.mode, "RUN_DIR=" + root, "KIND=" + filepath.Join(root, "kind"),
				"CLUSTER=owned-cluster", "NODE=owned-cluster-control-plane", "NODE_ID=owned-id",
				"CLUSTER_OWNED=1", "TOKEN=owned-token", "DRIVER_PID=", "RUN_PASSED=1",
				"KCFG=" + filepath.Join(root, "kubeconfig"),
			}
			output, err := shell(t, root, env, "-c", owns+scriptFunction(t, "cleanup"))
			require.Equal(t, tc.succeeded, err == nil, "cleanup: %s, %v", output, err)
			_, statErr := os.Stat(filepath.Join(root, "kind-called"))
			require.Equal(t, tc.deleted, statErr == nil, "kind deletion: %s", output)
			calls, err := os.ReadFile(filepath.Join(root, "docker-calls"))
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(string(calls), "ps -a --no-trunc --filter label=io.x-k8s.kind.cluster=owned-cluster --format {{.ID}}\n"), "cleanup did not first list all matching nodes: %s", calls)
			if tc.mode == "absent" {
				require.NotContains(t, string(calls), "inspect", "confirmed absence must not inspect a missing node")
			}
			result, err := os.ReadFile(filepath.Join(root, "runtime-result.json"))
			require.NoError(t, err)
			var recorded struct{ ExitCode int }
			require.NoError(t, json.Unmarshal(result, &recorded))
			require.Equal(t, tc.succeeded, recorded.ExitCode == 0, "incorrect cleanup result: %s", result)
		})
	}
}

func TestLauncherPreservesLeadingRunSubcommand(t *testing.T) {
	source, err := os.ReadFile("run.sh")
	require.NoError(t, err)
	_, launch, found := strings.Cut(string(source), "section 'Run scenarios'\n")
	require.True(t, found, "missing driver launch section")
	repository, err := filepath.Abs("../..")
	require.NoError(t, err)
	for _, args := range [][]string{
		{"run", "ordinary", "--auto"},
		{"ordinary", "--auto", "--process-group=false"},
	} {
		root := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(root, "bin"), 0700))
		testExecutable(t, filepath.Join(root, "bin"), "harness", `printf '%s\n' "$@" > "$RUN_DIR/driver-argv"
`)
		env := []string{
			"RUN_DIR=" + root, "KCFG=" + filepath.Join(root, "kubeconfig"),
			"CONTEXT=kind-owned", "NODE=owned-control-plane", "COMMIT=" + strings.Repeat("a", 40),
		}
		script := "set -euo pipefail\nSUITE_ARGS=(\"$@\")\n" + launch
		output, err := shell(t, root, env, append([]string{"-c", script, "launch-test"}, args...)...)
		require.NoError(t, err, "%s", output)
		raw, err := os.ReadFile(filepath.Join(root, "driver-argv"))
		require.NoError(t, err)
		actual := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
		require.Greater(t, len(actual), len(args))
		require.Equal(t, args, actual[:len(args)], "user arguments must remain first")
		require.Equal(t, "--process-group", actual[len(args)], "launcher isolation must override user flags")

		// Validate the actual launch argv with the real CLI, without ownership
		// checks, process-group changes, or any Kubernetes operations.
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		validateArgs := append([]string{"run", "./test/disaggregatedset_harness/cmd"}, actual...)
		command := exec.CommandContext(ctx, "go", append(validateArgs, "--validate")...)
		command.Dir = repository
		output, err = command.CombinedOutput()
		cancel()
		require.NoError(t, err, "actual launch arguments %q: %s", actual, output)
		require.Contains(t, string(output), "No Kubernetes operations performed")
	}
}

func TestLauncherKindConfigurationKeepsOwnedMount(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required for launcher JSON")
	}
	root := t.TempDir()
	output, err := shell(t, root, []string{"RUN_DIR=" + root, "HARNESS_MAX_PODS=160"}, "-c", scriptFunction(t, "kind_config"))
	require.NoError(t, err)
	var config Object
	require.NoError(t, json.Unmarshal(output, &config))
	patches := arr(config["kubeadmConfigPatches"])
	require.Len(t, patches, 1)
	require.Contains(t, str(patches[0]), "maxPods: 160")
	nodes := objects(config["nodes"])
	require.Len(t, nodes, 1)
	mounts := objects(nodes[0]["extraMounts"])
	require.Len(t, mounts, 1)
	require.Equal(t, filepath.Join(root, "owner"), mounts[0]["hostPath"])
	require.Equal(t, true, mounts[0]["readOnly"])
}

func TestLauncherUsesCheckedOutCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is required for checkout selection")
	}
	root := t.TempDir()
	git := func(t *testing.T, args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", append([]string{
			"-c", "user.name=Harness test", "-c", "user.email=harness@example.invalid",
			"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null",
		}, args...)...)
		command.Dir = root
		output, err := command.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, output)
		return strings.TrimSpace(string(output))
	}
	git(t, "init", "-q", "--initial-branch=main")
	git(t, "commit", "-q", "--allow-empty", "-m", "tagged commit")
	git(t, "-c", "tag.gpgsign=false", "tag", "snapshot")
	taggedCommit := git(t, "rev-parse", "HEAD")
	git(t, "commit", "-q", "--allow-empty", "-m", "branch checkout")
	branchCommit := git(t, "rev-parse", "HEAD")
	git(t, "checkout", "-q", "--detach")
	git(t, "commit", "-q", "--allow-empty", "-m", "detached checkout")
	detachedCommit := git(t, "rev-parse", "HEAD")
	for _, tc := range []struct {
		name, ref, commit string
	}{
		{"branch HEAD", "main", branchCommit},
		{"detached HEAD", detachedCommit, detachedCommit},
		{"tag checkout", "refs/tags/snapshot", taggedCommit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git(t, "checkout", "-q", tc.ref)
			env := []string{"REPOSITORY=" + root, "COMMIT=stale"}
			script := scriptFunction(t, "select_commit") + "printf '%s\\n' \"$COMMIT\"\n"
			output, err := shell(t, root, env, "-c", script)
			require.NoError(t, err, "%s", output)
			require.Equal(t, tc.commit+"\n", string(output))
		})
	}
}

func TestSourceBuildSupportsBothDockerBuilders(t *testing.T) {
	for _, provenance := range []string{"0", "1"} {
		root := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(root, "source"), 0700))
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
			"RUN_DIR=" + root, "COMMIT=" + commit, "ARCH=arm64", "IMAGE=local-controller:" + commit,
			"TEST_PROVENANCE=" + provenance}, "-c", scriptFunction(t, "prepare_image"))
		require.NoError(t, err)
		require.Equal(t, provenance == "1", strings.Contains(string(output), "--provenance=false"), "%s", output)
		require.Contains(t, string(output), "--platform linux/arm64")
		require.Contains(t, string(output), "--build-arg REVISION="+commit)
	}
}

func TestObserverOverlayIsExplicitAndDoesNotMutateSource(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required to prepare observer overlays")
	}
	prepare, err := filepath.Abs(filepath.Join("observer", "prepare.sh"))
	require.NoError(t, err)
	source, err := filepath.Abs("../..")
	require.NoError(t, err)
	root := t.TempDir()
	overlay := filepath.Join(root, "overlay.json")
	output, err := shell(t, root, nil, prepare, source, overlay)
	require.NoError(t, err, "%s", output)
	contents, err := os.ReadFile(overlay)
	require.NoError(t, err)
	var mapping struct{ Replace map[string]string }
	require.NoError(t, json.Unmarshal(contents, &mapping))
	require.NotEmpty(t, mapping.Replace)
	for virtual, backing := range mapping.Replace {
		require.True(t, strings.HasPrefix(virtual, source+string(os.PathSeparator)), "invalid overlay entry: %s -> %s", virtual, backing)
		_, err = os.Stat(virtual)
		require.ErrorIs(t, err, os.ErrNotExist, "overlay materialized a controller hook: %s", virtual)
		require.FileExists(t, backing)
	}
	output, err = shell(t, root, nil, prepare, source, overlay)
	require.Error(t, err, "%s", output)
	require.Contains(t, string(output), "already exists")
}

func TestObserverBuildFailureDoesNotFallback(t *testing.T) {
	root := t.TempDir()
	harness, err := filepath.Abs(".")
	require.NoError(t, err)
	prepare := filepath.Join(root, "observer")
	require.NoError(t, os.Mkdir(prepare, 0700))
	testExecutable(t, prepare, "prepare.sh", "exit 0\n")
	testExecutable(t, root, "go", "printf '%s\\n' \"$*\" >> \"$TEST_COMMAND_LOG\"\nexit 91\n")
	require.NoError(t, os.Mkdir(filepath.Join(root, "source"), 0700))
	log := filepath.Join(root, "commands.log")
	output, err := shell(t, harness, []string{"PATH=" + root + string(os.PathListSeparator) + os.Getenv("PATH"),
		"RUN_DIR=" + root, "HARNESS_DIR=" + root, "TEST_COMMAND_LOG=" + log}, "-c", scriptFunction(t, "build_observer"))
	require.Error(t, err, "accepted failed build: %s", output)
	contents, err := os.ReadFile(log)
	require.NoError(t, err)
	require.NotContains(t, strings.TrimSpace(string(contents)), "\n", "build retried with other semantics")
	require.Contains(t, string(contents), "-tags=harness ")
}
