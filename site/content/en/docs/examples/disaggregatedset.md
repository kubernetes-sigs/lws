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

Make sure the LWS controller manager is installed and running:

```shell
kubectl wait deploy/lws-controller-manager \
  -n lws-system --for=condition=available --timeout=5m
```

See the [installation guide](/docs/installation/#enabling-disaggregatedset) for setup instructions.

---

## Example 1 — Simple 2-Role (Prefill + Decode) Nginx

This example uses nginx containers to demonstrate a prefill + decode disaggregated topology without
requiring a real LLM. It closely mirrors the pattern used in production disaggregated inference.

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1
kind: DisaggregatedSet
metadata:
  name: disagg-nginx-demo
  namespace: default
spec:
  roles:
  # Prefill role: larger pool, higher parallelism
  - name: prefill
    spec:
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
    spec:
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

# Confirm both child LeaderWorkerSets were created using label selectors
kubectl get leaderworkerset -n default -l disaggregatedset.x-k8s.io/name=disagg-nginx-demo

# Expected output (revision hash in name is generated dynamically):
# NAME                                      REPLICAS   READY   AGE
# disagg-nginx-demo-58f79fdb78-prefill      2          2       30s
# disagg-nginx-demo-58f79fdb78-decode       1          1       30s

# Check all pods are running
kubectl get pods -n default -l component=disaggregation
```

---

## Example 2 — 3-Role LLM Inference Pattern

This example models a 3-phase disaggregated serving topology: prefill (KV cache generation),
decode (token generation), and encode (context encoding). It uses placeholder containers
(`registry.k8s.io/pause:3.9`) to demonstrate the scheduling topology without requiring GPU resources.

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1
kind: DisaggregatedSet
metadata:
  name: disagg-3role-demo
  namespace: default
spec:
  roles:
  # Prefill: generates KV cache from input tokens — CPU/GPU intensive
  - name: prefill
    spec:
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
    spec:
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
    spec:
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

# All three child LWS resources should appear (revision hash generated dynamically)
kubectl get leaderworkerset -n default -l disaggregatedset.x-k8s.io/name=disagg-3role-demo
# NAME                                   REPLICAS   READY   AGE
# disagg-3role-demo-58f79fdb78-prefill   4          4       30s
# disagg-3role-demo-58f79fdb78-decode    2          2       30s
# disagg-3role-demo-58f79fdb78-encode    2          2       30s
```

---

## Understanding Child LWS Names

The DisaggregatedSet controller names each child `LeaderWorkerSet` using a **revision hash** to track
rollouts. The naming format is:

```
<DisaggregatedSet-name>-<revision-hash>-<role-name>
```

For example, a `DisaggregatedSet` named `my-inference` with roles `prefill` and `decode` creates:
- `my-inference-58f79fdb78-prefill`
- `my-inference-58f79fdb78-decode`

> **Note:** The revision hash is dynamic and changes on each rollout. Never rely on hardcoded
> child LWS names — always use label selectors to query them.

You can list all child LWS resources for a given DisaggregatedSet with:

```shell
kubectl get leaderworkerset -l disaggregatedset.x-k8s.io/name=my-inference
```

To filter by role:

```shell
kubectl get leaderworkerset -l disaggregatedset.x-k8s.io/name=my-inference,disaggregatedset.x-k8s.io/role=prefill
```

## Checking Status

```shell
# Check the DisaggregatedSet overall status
kubectl describe disaggregatedset disagg-nginx-demo

# Check the status of child LWSes by label
kubectl get leaderworkerset -l disaggregatedset.x-k8s.io/name=disagg-nginx-demo \
  -l disaggregatedset.x-k8s.io/role=prefill
```

## Cleanup

```shell
# Deleting the DisaggregatedSet also deletes all child LeaderWorkerSets
kubectl delete disaggregatedset disagg-nginx-demo disagg-3role-demo
```
