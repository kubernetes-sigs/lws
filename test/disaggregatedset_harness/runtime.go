// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Executor keeps process invocation replaceable in offline and integration tests.
// Arguments are passed directly, never interpreted by a shell.
type Executor interface {
	Run(context.Context, string, []string, []byte) ([]byte, []byte, error)
}
type OSExecutor struct{}

func (OSExecutor) Run(ctx context.Context, program string, args []string, input []byte) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Stdin = bytes.NewReader(input)
	var out, errout bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errout
	err := cmd.Run()
	return out.Bytes(), errout.Bytes(), err
}

type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

func (RealClock) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type Options struct {
	Kubeconfig, Context, RunDir, Node, ControllerCommit, Observer string
	Kubectl                                                       string
	Auto, FailFast                                                bool
	Output                                                        io.Writer
	Input                                                         io.Reader
	Executor                                                      Executor
	Clock                                                         Clock
}

func (o *Options) defaults() {
	if o.Kubectl == "" {
		o.Kubectl = "kubectl"
	}
	if o.Output == nil {
		o.Output = os.Stdout
	}
	if o.Input == nil {
		o.Input = os.Stdin
	}
	if o.Executor == nil {
		o.Executor = OSExecutor{}
	}
	if o.Clock == nil {
		o.Clock = RealClock{}
	}
}

func (o Options) kube(args ...string) []string {
	return append([]string{"--kubeconfig", o.Kubeconfig, "--context", o.Context, "--request-timeout=15s"}, args...)
}

// ValidateOwnership is read-only and must succeed before ExecuteSuite. It checks
// the private kubeconfig and runtime, exact Docker node identity, kind label,
// and the read-only token mount. Names alone are never proof of ownership.
func ValidateOwnership(ctx context.Context, o Options) error {
	o.defaults()
	root, err := filepath.Abs(o.RunDir)
	if err != nil {
		return err
	}
	kcfg, err := filepath.Abs(o.Kubeconfig)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(o.Context, "kind-lws-v1-run-") || o.Node != strings.TrimPrefix(o.Context, "kind-")+"-control-plane" || kcfg != filepath.Join(root, "kubeconfig") || o.Observer == "" || o.ControllerCommit == "" {
		return fmt.Errorf("refusing a context or path outside an owned private harness run")
	}
	b, err := os.ReadFile(filepath.Join(root, "runtime.json"))
	if err != nil {
		return err
	}
	var proof Object
	if err = json.Unmarshal(b, &proof); err != nil {
		return err
	}
	owner, err := os.ReadFile(filepath.Join(root, "owner"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(owner)) == "" || strings.TrimSpace(string(owner)) != str(proof["owner"]) || proof["cluster"] != strings.TrimPrefix(o.Context, "kind-") || proof["node"] != o.Node || proof["controllerCommit"] != o.ControllerCommit || str(proof["nodeID"]) == "" {
		return fmt.Errorf("runtime ownership identity does not match this run")
	}
	read := func(program string, args ...string) ([]byte, error) {
		call, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		out, stderr, e := o.Executor.Run(call, program, args, nil)
		if e != nil {
			return nil, fmt.Errorf("ownership check: %s: %w: %s", program, e, stderr)
		}
		return out, nil
	}
	node, err := read("docker", "inspect", "--type", "container", o.Node)
	if err != nil {
		return err
	}
	var nodes []Object
	if err = json.Unmarshal(node, &nodes); err != nil || len(nodes) != 1 {
		return fmt.Errorf("cannot verify exactly one Docker node")
	}
	mount := ""
	for _, m := range objects(nodes[0]["Mounts"]) {
		if m["Destination"] == "/var/run/lws-v1-harness-owner" && m["RW"] == false {
			mount = str(m["Source"])
		}
	}
	if !OwnsNode(str(nodes[0]["Id"]), str(at(nodes[0], "Config", "Labels", "io.x-k8s.kind.cluster")), mount, str(proof["nodeID"]), str(proof["cluster"]), filepath.Join(root, "owner"), runtime.GOOS) {
		return fmt.Errorf("Docker node ownership no longer matches; refusing mutations")
	}
	config, err := read(o.Kubectl, o.kube("config", "view", "-o", "json")...)
	if err != nil {
		return err
	}
	var raw Object
	if err = json.Unmarshal(config, &raw); err != nil {
		return err
	}
	contexts := objects(raw["contexts"])
	clusters := objects(raw["clusters"])
	if raw["current-context"] != o.Context || len(contexts) != 1 || contexts[0]["name"] != o.Context || len(clusters) != 1 || at(contexts[0], "context", "cluster") != clusters[0]["name"] {
		return fmt.Errorf("private kubeconfig must contain exactly this selected context and cluster")
	}
	endpoint, err := url.Parse(str(at(clusters[0], "cluster", "server")))
	if err != nil || endpoint.Scheme != "https" || (endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "::1") {
		return fmt.Errorf("refusing non-local Kubernetes endpoint")
	}
	bound := false
	for _, binding := range objects(at(nodes[0], "NetworkSettings", "Ports", "6443/tcp")) {
		if binding["HostIp"] == endpoint.Hostname() && binding["HostPort"] == endpoint.Port() {
			bound = true
		}
	}
	if !bound {
		return fmt.Errorf("private kubeconfig endpoint does not match the owned node's published API port")
	}
	return nil
}

func OwnsNode(id, label, mount, expectedID, cluster, ownerPath, goos string) bool {
	return id != "" && id == expectedID && label == cluster && (mount == ownerPath || (goos == "darwin" && mount == "/host_mnt"+ownerPath))
}

type failure struct{ err error }

func abort(err error) {
	if err != nil {
		panic(failure{err})
	}
}

func writeJSON(path string, value any) error {
	b, e := json.MarshalIndent(value, "", "  ")
	if e != nil {
		return e
	}
	return os.WriteFile(path, append(b, '\n'), 0600)
}
