---
title: "DisaggregatedSet Examples"
linkTitle: "DisaggregatedSet"
weight: 30
description: >
  Minimal working examples for DisaggregatedSet — from a simple 2-role nginx setup to a 3-role LLM inference pattern.
---

<!-- toc -->
- [Before You Begin](#before-you-begin)
- [Example 1 — Simple 2-Role (Prefill + Decode) Nginx](#example-1--simple-2-role-prefill--decode-nginx)
  - [Apply and Verify](#apply-and-verify)
- [Example 2 — 3-Role LLM Inference Pattern](#example-2--3-role-llm-inference-pattern)
  - [Apply and Verify](#apply-and-verify-1)
- [Understanding Child LWS Names](#understanding-child-lws-names)
- [Checking Status](#checking-status)
- [Cleanup](#cleanup)
<!-- /toc -->

## Before You Begin

Make sure both LWS and the DisaggregatedSet controller are installed and running:

```shell
kubectl wait deploy/lws-controller-manager \
  -n lws-system --for=condition=available --timeout=5m

kubectl wait deploy/disaggregatedset-controller-manager \
  -n disaggregatedset-system --for=condition=available --timeout=5m
```

See the [installation guide](/docs/installation/disaggregatedset/) for setup instructions.

---

## Example 1 — Simple 2-Role (Prefill + Decode) Nginx

This example uses nginx containers to demonstrate a prefill + decode disaggregated topology without
requiring a real LLM. It closely mirrors the pattern used in production disaggregated inference.

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1alpha1
kind: DisaggregatedSet
metadata:
  name: disagg-nginx-demo
  namespace: default
spec:
  roles:
  # Prefill role: larger pool, higher parallelism
  - name: prefill
    replicas: 2
    rolloutStrategy:
      rollingUpdateConfiguration:
        maxSurge: 0
        maxUnavailable: 1
    leaderWorkerTemplate:
      size: 2          # 1 leader + 1 worker per group
      workerTemplate:
        metadata:
          labels:
            role: prefill
            component: disaggregation
        spec:
          containers:
          - name: nginx
            image: nginx:1.29.3
            ports:
            - containerPort: 80
            resources:
              requests:
                cpu: "100m"
                memory: "64Mi"
              limits:
                cpu: "100m"
                memory: "64Mi"
            readinessProbe:
              httpGet:
                path: /
                port: 80
              initialDelaySeconds: 5
              periodSeconds: 2

  # Decode role: smaller pool, lower latency
  - name: decode
    replicas: 1
    rolloutStrategy:
      rollingUpdateConfiguration:
        maxSurge: 1
        maxUnavailable: 0
    leaderWorkerTemplate:
      size: 1          # 1 leader only per group
      workerTemplate:
        metadata:
          labels:
            role: decode
            component: disaggregation
        spec:
          containers:
          - name: nginx
            image: nginx:1.29.3
            ports:
            - containerPort: 80
            resources:
              requests:
                cpu: "100m"
                memory: "64Mi"
              limits:
                cpu: "100m"
                memory: "64Mi"
            readinessProbe:
              httpGet:
                path: /
                port: 80
              initialDelaySeconds: 5
              periodSeconds: 2
```

### Apply and Verify

```shell
# Apply the manifest
kubectl apply -f disagg-nginx-demo.yaml

# Confirm both child LeaderWorkerSets were created
kubectl get leaderworkerset -n default

# Expected output:
# NAME                          REPLICAS   READY   AGE
# disagg-nginx-demo-prefill     2          2       30s
# disagg-nginx-demo-decode      1          1       30s

# Check all pods are running
kubectl get pods -n default -l component=disaggregation
```

---

## Example 2 — 3-Role LLM Inference Pattern

This example models a 3-phase disaggregated serving topology: prefill (KV cache generation),
decode (token generation), and encode (context encoding). It uses placeholder containers
(`registry.k8s.io/pause:3.9`) to demonstrate the scheduling topology without requiring GPU resources.

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1alpha1
kind: DisaggregatedSet
metadata:
  name: disagg-3role-demo
  namespace: default
spec:
  roles:
  # Prefill: generates KV cache from input tokens — CPU/GPU intensive
  - name: prefill
    replicas: 4
    rolloutStrategy:
      rollingUpdateConfiguration:
        maxSurge: 1
        maxUnavailable: 0
    leaderWorkerTemplate:
      size: 2
      workerTemplate:
        metadata:
          labels:
            role: prefill
        spec:
          containers:
          - name: model
            image: registry.k8s.io/pause:3.9
            resources:
              requests:
                cpu: "100m"
                memory: "64Mi"
              limits:
                cpu: "100m"
                memory: "64Mi"

  # Decode: generates tokens autoregressively — memory-bandwidth intensive
  - name: decode
    replicas: 2
    rolloutStrategy:
      rollingUpdateConfiguration:
        maxSurge: 1
        maxUnavailable: 0
    leaderWorkerTemplate:
      size: 1
      workerTemplate:
        metadata:
          labels:
            role: decode
        spec:
          containers:
          - name: model
            image: registry.k8s.io/pause:3.9
            resources:
              requests:
                cpu: "100m"
                memory: "64Mi"
              limits:
                cpu: "100m"
                memory: "64Mi"

  # Encode: context encoding — optional, separate scaling
  - name: encode
    replicas: 2
    rolloutStrategy:
      rollingUpdateConfiguration:
        maxSurge: 1
        maxUnavailable: 0
    leaderWorkerTemplate:
      size: 1
      workerTemplate:
        metadata:
          labels:
            role: encode
        spec:
          containers:
          - name: model
            image: registry.k8s.io/pause:3.9
            resources:
              requests:
                cpu: "100m"
                memory: "64Mi"
              limits:
                cpu: "100m"
                memory: "64Mi"
```

### Apply and Verify

```shell
kubectl apply -f disagg-3role-demo.yaml

# All three child LWS resources should appear
kubectl get leaderworkerset -n default
# NAME                         REPLICAS   READY   AGE
# disagg-3role-demo-prefill    4          4       30s
# disagg-3role-demo-decode     2          2       30s
# disagg-3role-demo-encode     2          2       30s
```

---

## Understanding Child LWS Names

The DisaggregatedSet controller names each child `LeaderWorkerSet` as:

```
<DisaggregatedSet-name>-<role-name>
```

For a `DisaggregatedSet` named `my-inference` with roles `prefill` and `decode`, the controller creates:
- `my-inference-prefill`
- `my-inference-decode`

You can list them with:

```shell
kubectl get leaderworkerset -l disaggregatedset.x-k8s.io/name=my-inference
```

## Checking Status

```shell
# Check the DisaggregatedSet overall status
kubectl describe disaggregatedset disagg-nginx-demo

# Check the status of a specific child LWS
kubectl describe leaderworkerset disagg-nginx-demo-prefill
```

## Cleanup

```shell
# Deleting the DisaggregatedSet also deletes all child LeaderWorkerSets
kubectl delete disaggregatedset disagg-nginx-demo disagg-3role-demo
```
