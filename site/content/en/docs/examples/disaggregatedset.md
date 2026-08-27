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
- [Operating a DisaggregatedSet](#operating-a-disaggregatedset)
  - [Scaling slices](#scaling-slices)
  - [Scaling replicas within a role](#scaling-replicas-within-a-role)
  - [Rolling updates](#rolling-updates)
  - [Placement policy](#placement-policy)
  - [External scaling with HPA or KEDA](#external-scaling-with-hpa-or-keda)
- [Cleanup](#cleanup)
<!-- /toc -->

## Before You Begin

Make sure the LWS controller manager is installed and running:

```shell
kubectl wait deploy/lws-controller-manager \
  -n lws-system --for=condition=available --timeout=5m
```

See the [installation guide](/docs/installation/#disaggregatedset) for setup instructions.

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
# NAME                                        REPLICAS   READY   AGE
# disagg-nginx-demo-0-58f79fdb78-prefill      2          2       30s
# disagg-nginx-demo-0-58f79fdb78-decode       1          1       30s

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
# NAME                                     REPLICAS   READY   AGE
# disagg-3role-demo-0-58f79fdb78-prefill   4          4       30s
# disagg-3role-demo-0-58f79fdb78-decode    2          2       30s
# disagg-3role-demo-0-58f79fdb78-encode    2          2       30s
```

---

## Understanding Child LWS Names

The DisaggregatedSet controller names each child `LeaderWorkerSet` using a **slice index** and a
**revision hash** to track rollouts. The naming format is:

```
<DisaggregatedSet-name>-<slice>-<revision-hash>-<role-name>
```

For example, a `DisaggregatedSet` named `my-inference` with roles `prefill` and `decode` (and the
default single slice) creates:
- `my-inference-0-58f79fdb78-prefill`
- `my-inference-0-58f79fdb78-decode`

> **Note:** The revision hash is dynamic and changes on each rollout. Never rely on hardcoded
> child LWS names — always use label selectors to query them. Controllers at `v0.9.0` or older
> predate slices and omit the `<slice>` segment.

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

## Operating a DisaggregatedSet

> **Note:** `spec.slices`, `spec.placementPolicy`, and `scaling.mode: External` were added after
> the `v0.9.0` release and ship in the next release. Controllers at `v0.9.0` or older reject
> these fields.

### Scaling slices

`spec.slices` replicates the entire role topology into N independent copies. Each slice is a
complete set of all roles with its own rollout clock and a stable identity
(`disaggregatedset.x-k8s.io/slice` label). Because `slices` is excluded from the revision hash,
changing it is a pure scale operation: new slices come up at the current revision and existing
slices are never touched.

```shell
# Add a third complete copy of the topology
kubectl patch disaggregatedset disagg-nginx-demo --type merge -p '{"spec":{"slices":3}}'
```

Scaling down deletes the highest-indexed slices' resources directly. Lower slices are untouched.

### Scaling replicas within a role

Per-role `spec.replicas` is a **per-slice** count: the role runs that many groups in every slice,
so the ratio between roles is defined once and holds in each slice. With 2 slices, raising prefill
replicas from 2 to 3 yields 6 prefill groups in total.

```shell
kubectl patch disaggregatedset disagg-nginx-demo --type json \
  -p '[{"op": "replace", "path": "/spec/roles/0/spec/replicas", "value": 3}]'
```

### Rolling updates

Any change to a role's pod template creates a new revision, and each slice rolls to it
independently, always keeping a complete same-version set of all roles serving per slice. While a
slice is mid-rollout you will see two revisions of its LWS at once (old draining, new filling). A
stuck slice degrades only itself. Watch a single slice with:

```shell
kubectl get leaderworkerset \
  -l disaggregatedset.x-k8s.io/name=disagg-nginx-demo,disaggregatedset.x-k8s.io/slice=0 -w
```

### Placement policy

`spec.placementPolicy` confines each slice to a single topology domain and spreads slices across
domains, so cross-role traffic (for example prefill-to-decode KV-cache transfer) stays within a
low-latency domain:

```yaml
spec:
  placementPolicy:
    type: ExclusiveSlice  # or ExclusiveTopology for a 1:1 domain-to-slice mapping across all DisaggregatedSets
    topology: topology.kubernetes.io/zone  # any node-label key: zone, rack, NVLink domain, ...
```

`ExclusiveSlice` co-locates each slice in one domain. `ExclusiveTopology` additionally guarantees
at most one slice per domain across all DisaggregatedSets. The controller injects the affinity
when it creates a LeaderWorkerSet, so changing the policy takes effect on each slice's next
rollout. See [KEP-848](https://github.com/kubernetes-sigs/lws/tree/main/keps/848-disaggregatedset-placement-policy)
for the full design.

### External scaling with HPA or KEDA

By default a role's replica count is static, coming from its `spec.replicas`. Setting
`scaling.mode: External` on a role delegates the count to an external autoscaler instead:

```yaml
spec:
  roles:
  - name: prefill
    scaling:
      mode: External   # spec.replicas is ignored for this role
    spec:
      ...
```

The controller auto-creates a `DisaggregatedSetRoleScaler` named `<DisaggregatedSet-name>-<role-name>`
that exposes the `/scale` subresource. Point an HPA (or KEDA ScaledObject) at it:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: disagg-nginx-demo-prefill
spec:
  scaleTargetRef:
    apiVersion: disaggregatedset.x-k8s.io/v1
    kind: DisaggregatedSetRoleScaler
    name: disagg-nginx-demo-prefill
  minReplicas: 2
  maxReplicas: 8
  metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 50
```

The autoscaler writes the desired count through `/scale`, and the controller drives the role's
LeaderWorkerSets from it. The scaler's `status.selector` matches one pod per group (the leader),
so per-pod metric averaging divides by the group count and the ratio math stays consistent for
multi-pod groups. See [KEP-849](https://github.com/kubernetes-sigs/lws/tree/main/keps/849-DisaggregatedSet-HPA)
for the full design.

Resource metrics like the CPU example above require metrics-server. See the
[HPA example's metrics-server setup](/docs/examples/hpa/#1-install-metrics-server) (that page
targets a standalone LeaderWorkerSet, while a DisaggregatedSet role is scaled through its
DisaggregatedSetRoleScaler as shown here).

> **Note:** `scaling.mode: External` currently requires the default single slice
> (`spec.slices: 1`). Multi-slice support, where the scaler value is the total across all
> slices, is proposed in
> [KEP-948](https://github.com/kubernetes-sigs/lws/issues/948).

## Cleanup

```shell
# Deleting the DisaggregatedSet also deletes all child LeaderWorkerSets
kubectl delete disaggregatedset disagg-nginx-demo disagg-3role-demo
```
