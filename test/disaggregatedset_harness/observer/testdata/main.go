//go:build harness

/*
Copyright 2026 The Kubernetes Authors.

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

// Command observer prints read-only, independent diagnostic snapshots as JSONL.
// It is not part of the controller and cannot authorize or execute a rollout.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	disaggcontroller "sigs.k8s.io/lws/pkg/controllers/disaggregatedset"
)

func main() {
	// Imported controller-runtime packages register their own global flags.
	flags := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	kubeconfig := flags.String("kubeconfig", "", "isolated kind kubeconfig (required)")
	selectedContext := flags.String("context", "", "exact kind context (required)")
	namespace := flags.String("namespace", "v1-harness", "namespace to observe")
	name := flags.String("name", "", "DisaggregatedSet name (required)")
	watch := flags.Float64("watch", 0, "repeat every N seconds; zero prints one snapshot")
	_ = flags.Parse(os.Args[1:])
	if *kubeconfig == "" || !strings.HasPrefix(*selectedContext, "kind-") || *name == "" || *watch < 0 {
		fatal(fmt.Errorf("--kubeconfig, --context kind-NAME and --name are required; --watch must be nonnegative"))
	}
	config, err := isolatedConfig(*kubeconfig, *selectedContext)
	if err != nil {
		fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reader, err := startReader(ctx, config, *namespace)
	if err != nil {
		fatal(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	key := client.ObjectKey{Namespace: *namespace, Name: *name}
	for {
		snapshot, err := disaggcontroller.HarnessObserve(ctx, reader, key)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if *watch == 0 {
				fatal(err)
			}
			snapshot = map[string]any{"schemaVersion": 1, "observedAt": time.Now().UTC().Format(time.RFC3339Nano), "error": err.Error()}
		}
		if err := encoder.Encode(snapshot); err != nil {
			fatal(err)
		}
		if *watch == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(*watch * float64(time.Second))):
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func isolatedConfig(path, selected string) (*rest.Config, error) {
	raw, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw.Contexts) != 1 || raw.Contexts[selected] == nil || raw.CurrentContext != selected {
		return nil, fmt.Errorf("kubeconfig must contain only the selected current context %q", selected)
	}
	config, err := clientcmd.NewNonInteractiveClientConfig(*raw, selected, nil, nil).ClientConfig()
	if err != nil {
		return nil, err
	}
	endpoint, err := url.Parse(config.Host)
	if err != nil || endpoint.Scheme != "https" || (endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "::1") {
		return nil, fmt.Errorf("refusing non-local Kubernetes endpoint")
	}
	config.UserAgent = "lws-v1-harness-readonly-observer"
	return config, nil
}

// Eagerly start all required informers once, avoiding a fresh cache at each poll.
// The returned interface has no mutation methods.
func startReader(ctx context.Context, config *rest.Config, namespace string) (client.Reader, error) {
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, disaggv1.AddToScheme, leaderv1.AddToScheme} {
		if err := register(scheme); err != nil {
			return nil, err
		}
	}
	reader, err := cache.New(config, cache.Options{Scheme: scheme, DefaultNamespaces: map[string]cache.Config{namespace: {}}, ReaderFailOnMissingInformer: true})
	if err != nil {
		return nil, err
	}
	for _, object := range []client.Object{&disaggv1.DisaggregatedSet{}, &disaggv1.DisaggregatedSetRoleScaler{}, &leaderv1.LeaderWorkerSet{},
		&corev1.Pod{}, &appsv1.StatefulSet{}, &appsv1.Deployment{}, &appsv1.ReplicaSet{}} {
		if _, err := reader.GetInformer(ctx, object, cache.BlockUntilSynced(false)); err != nil {
			return nil, err
		}
	}
	go func() {
		if err := reader.Start(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "observer cache:", err)
		}
	}()
	startup, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if !reader.WaitForCacheSync(startup) {
		return nil, fmt.Errorf("observer informers did not synchronize within 45 seconds")
	}
	return reader, nil
}
