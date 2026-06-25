---
title: "DisaggregatedSet"
linkTitle: "DisaggregatedSet"
weight: 60
description: >
  A minimal example deploying a two-role disaggregated inference workload using DisaggregatedSet.
---

This example shows how to deploy a minimal two-role (prefill/decode) workload using
`DisaggregatedSet`. It uses nginx as a placeholder container — in production you would
replace this with your inference server image (e.g. vLLM, SGLang, NVIDIA Dynamo).

For the full background on why disaggregated serving improves LLM throughput, see
[DisaggregatedSet Concepts](../concepts/disaggregatedset).

---

## Prerequisites

- LWS v0.9.0 or later installed (see [Installation](../installation/)).
- `kubectl` configured against your cluster.

---

## Minimal Two-Role Example

The `DisaggregatedSet` below creates:
- A **prefill** pool with 2 LWS replicas, each with a 1-node leader+worker group.
- A **decode** pool with 2 LWS replicas, each with a 1-node leader+worker group.

The controller automatically creates two `LeaderWorkerSet` resources named
`disaggregatedset-sample-{revision}-prefill` and `disaggregatedset-sample-{revision}-decode`,
plus per-revision headless Services for each role.

```yaml
apiVersion: disaggregatedset.x-k8s.io/v1
kind: DisaggregatedSet
metadata:
  name: disaggregatedset-sample
  namespace: default
spec:
  roles:
    - name: prefill
      spec:
        replicas: 2
        leaderWorkerTemplate:
          size: 1
          leaderTemplate:
            metadata:
              labels:
                role: prefill
            spec:
              containers:
                - name: worker
                  image: nginx:latest
                  ports:
                    - containerPort: 80
          workerTemplate:
            metadata:
              labels:
                role: prefill
            spec:
              containers:
                - name: worker
                  image: nginx:latest
                  ports:
                    - containerPort: 80
    - name: decode
      spec:
        replicas: 2
        leaderWorkerTemplate:
          size: 1
          leaderTemplate:
            metadata:
              labels:
                role: decode
            spec:
              containers:
                - name: worker
                  image: nginx:latest
                  ports:
                    - containerPort: 80
          workerTemplate:
            metadata:
              labels:
                role: decode
            spec:
              containers:
                - name: worker
                  image: nginx:latest
                  ports:
                    - containerPort: 80
```

Apply it with:

```shell
kubectl apply -f https://github.com/kubernetes-sigs/lws/blob/main/config/samples/disaggregatedset_v1_disaggregatedset.yaml
```

---

## Verify the Deployment

### Check DisaggregatedSet status

```shell
kubectl get disaggregatedsets disaggregatedset-sample
```

Expected output (after all pods are ready):

```
NAME                      AGE
disaggregatedset-sample   30s
```

For detailed status including per-role readiness:

```shell
kubectl describe disaggregatedsets disaggregatedset-sample
```

The `Status.RoleStatuses` section will show `replicas`, `readyReplicas`, and `updatedReplicas` for each role.

### Inspect managed LeaderWorkerSets

```shell
kubectl get leaderworkersets \
  -l disaggregatedset.x-k8s.io/name=disaggregatedset-sample
```

You will see one `LeaderWorkerSet` per role (and per active revision during a rolling update):

```
NAME                                        REPLICAS   READY   AGE
disaggregatedset-sample-abc12345-prefill    2          2       30s
disaggregatedset-sample-abc12345-decode     2          2       30s
```

### List pods by role

```shell
# Prefill pods
kubectl get pods -l disaggregatedset.x-k8s.io/role=prefill

# Decode pods
kubectl get pods -l disaggregatedset.x-k8s.io/role=decode
```

### Inspect headless Services

```shell
kubectl get svc \
  -l disaggregatedset.x-k8s.io/name=disaggregatedset-sample
```

Each role gets a headless service per revision:

```
NAME                                               TYPE        CLUSTER-IP   EXTERNAL-IP   PORT(S)   AGE
disaggregatedset-sample-abc12345-prefill-prv       ClusterIP   None         <none>        80/TCP    30s
disaggregatedset-sample-abc12345-decode-prv        ClusterIP   None         <none>        80/TCP    30s
```

---

## Three-Role Example

For workloads that require an additional role (e.g., encode, prefill, decode), apply the three-role sample:

```shell
kubectl apply -f https://github.com/kubernetes-sigs/lws/blob/main/config/samples/disaggregatedset_v1_3role.yaml
```

This sample configures `maxSurge: 1` and `maxUnavailable: 0` per role, ensuring zero-downtime
rolling updates where new replicas are always ready before old ones are removed.

---

## Clean Up

```shell
kubectl delete disaggregatedsets disaggregatedset-sample
```

All owned `LeaderWorkerSet` resources and headless `Services` are garbage-collected automatically.
