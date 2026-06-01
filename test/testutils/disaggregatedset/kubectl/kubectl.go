/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubectl

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
)

// Builder provides a fluent interface for constructing kubectl commands.
type Builder struct {
	cmd           string
	resource      string
	name          string
	namespace     string
	labels        []string // label selectors as "key=value" or "key!=value"
	fieldSelector string
	output        string
	flags         []string
	stdin         string
	quiet         bool
}

// Get starts a kubectl get command.
func Get(resource string) *Builder {
	return &Builder{cmd: "get", resource: resource}
}

// Delete starts a kubectl delete command.
func Delete(resource string, name ...string) *Builder {
	b := &Builder{cmd: "delete", resource: resource}
	if len(name) > 0 {
		b.name = name[0]
	}
	return b
}

// Apply starts a kubectl apply command with stdin input.
func Apply(yaml string) *Builder {
	return &Builder{cmd: "apply", stdin: yaml, flags: []string{"-f", "-"}}
}

// Create starts a kubectl create command.
func Create(resource string, name ...string) *Builder {
	b := &Builder{cmd: "create", resource: resource}
	if len(name) > 0 {
		b.name = name[0]
	}
	return b
}

// Label adds a label selector (key=value).
func (b *Builder) Label(key, value string) *Builder {
	b.labels = append(b.labels, fmt.Sprintf("%s=%s", key, value))
	return b
}

// LabelNot adds a label not-equal selector (key!=value).
func (b *Builder) LabelNot(key, value string) *Builder {
	b.labels = append(b.labels, fmt.Sprintf("%s!=%s", key, value))
	return b
}

// Namespace sets the namespace.
func (b *Builder) Namespace(ns string) *Builder {
	b.namespace = ns
	return b
}

// JSONPath sets output format to jsonpath with the given template.
func (b *Builder) JSONPath(template string) *Builder {
	b.output = fmt.Sprintf("jsonpath=%s", template)
	return b
}

// Output sets the output format.
func (b *Builder) Output(format string) *Builder {
	b.output = format
	return b
}

// FieldSelector adds a field selector.
func (b *Builder) FieldSelector(selector string) *Builder {
	b.fieldSelector = selector
	return b
}

// IgnoreNotFound adds --ignore-not-found flag.
func (b *Builder) IgnoreNotFound() *Builder {
	b.flags = append(b.flags, "--ignore-not-found")
	return b
}

// Timeout adds --timeout flag.
func (b *Builder) Timeout(duration string) *Builder {
	b.flags = append(b.flags, "--timeout="+duration)
	return b
}

// NoHeaders adds --no-headers flag.
func (b *Builder) NoHeaders() *Builder {
	b.flags = append(b.flags, "--no-headers")
	return b
}

// Force adds --force flag.
func (b *Builder) Force() *Builder {
	b.flags = append(b.flags, "--force")
	return b
}

// GracePeriod adds --grace-period flag.
func (b *Builder) GracePeriod(seconds int) *Builder {
	b.flags = append(b.flags, fmt.Sprintf("--grace-period=%d", seconds))
	return b
}

// DryRun adds --dry-run=client flag.
func (b *Builder) DryRun() *Builder {
	b.flags = append(b.flags, "--dry-run=client")
	return b
}

// Quiet suppresses logging when running.
func (b *Builder) Quiet() *Builder {
	b.quiet = true
	return b
}

// buildArgs constructs the kubectl arguments.
func (b *Builder) buildArgs() []string {
	args := []string{b.cmd}

	if b.resource != "" {
		args = append(args, b.resource)
	}
	if b.name != "" {
		args = append(args, b.name)
	}

	if len(b.labels) > 0 {
		args = append(args, "-l", strings.Join(b.labels, ","))
	}

	if b.namespace != "" {
		args = append(args, "-n", b.namespace)
	}
	if b.fieldSelector != "" {
		args = append(args, "--field-selector="+b.fieldSelector)
	}
	if b.output != "" {
		args = append(args, "-o", b.output)
	}

	args = append(args, b.flags...)
	return args
}

// Run executes the kubectl command and returns the output.
func (b *Builder) Run() (string, error) {
	args := b.buildArgs()
	cmd := exec.Command("kubectl", args...)

	if b.stdin != "" {
		cmd.Stdin = strings.NewReader(b.stdin)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")

	if !b.quiet {
		_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", strings.Join(cmd.Args, " "))
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed: %s: %w", strings.Join(cmd.Args, " "), string(output), err)
	}
	return string(output), nil
}

// RunQuiet executes without logging.
func (b *Builder) RunQuiet() (string, error) {
	b.quiet = true
	return b.Run()
}
