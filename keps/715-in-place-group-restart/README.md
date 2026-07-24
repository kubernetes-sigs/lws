# KEP-715: In-Place Group Restart for LeaderWorkerSet

<!-- toc -->
- [2. Executive Summary](#2-executive-summary)
- [4. Architectural Design (True Group Restart)](#4-architectural-design-true-group-restart)
  - [4.1 The <code>RestartAllContainers</code> Primitive](#41-the--primitive)
  - [4.2 The Credential-Less Restartable Init Container Agent](#42-the-credential-less-restartable-init-container-agent)
  - [4.3 Coordination via Leader Pod Annotations](#43-coordination-via-leader-pod-annotations)
- [6. Controller State Machine &amp; Deduplication](#6-controller-state-machine--deduplication)
  - [6.1 The Triggering Pod Handshake](#61-the-triggering-pod-handshake)
  - [6.2 Controller Phases (Leader Pod)](#62-controller-phases-leader-pod)
- [8. Requirements on Workload Images](#8-requirements-on-workload-images)
- [10. Feature Gates &amp; Kubernetes Compatibility](#10-feature-gates--kubernetes-compatibility)
- [11. Test Plan](#11-test-plan)
<!-- /toc -->

---

## 1. Metadata & Status

- **KEP ID:** 715
- **Title:** In-Place Group Restart for LeaderWorkerSet
- **Authors:** @Dasmat13
- **Owning SIG:** `sig-apps`
- **Target Release:** v0.10.0 (Alpha), v0.11.0 (Beta)
- **Status:** Proposed
- **Issue Reference:** [kubernetes-sigs/lws#715](https://github.com/kubernetes-sigs/lws/issues/715)
- **Minimum Kubernetes Version:** 1.36+ (Requires `RestartAllContainersOnContainerExits`)

---

## 2. Executive Summary

`LeaderWorkerSet` (LWS) currently handles container failures via the `RecreateGroupOnPodRestart` policy, which deletes and reschedules all $M+1$ pods in a group. For large GPU training workloads, this infrastructure churn creates massive delays (IP reassignment, topology re-evaluation, image pulls).

This proposal implements a true **In-Place Group Restart** leveraging the Kubernetes 1.36 `RestartAllContainers` primitive. 

> [!NOTE]
> This feature restarts the *entire group* (every container on every pod in the group) synchronously. It saves K8s-level scheduling churn (IP reassignment, device rebinding), but the application processes (e.g., PyTorch, Ray) will still undergo a full application-level initialization.

When a specified workload container crashes with a recoverable exit code, the Kubelet natively triggers a pod-level restart. The LWS controller then centralizes a new restart generation on the **Leader Pod**, and mirrors it to all sibling pods via Downward API volumes. An injected, credential-less sidecar (`lws-restart-agent`) observes this signal, commands healthy peers to restart, and acts as a startup/readiness barrier to ensure all peers re-rendezvous simultaneously.

---

## 3. Motivation & Problem Statement

Currently, LWS forces users into two bad choices for failures:
1. **Full Recreation:** `RecreateGroupOnPodRestart` tears down the entire StatefulSet, releasing IP addresses and device allocations.
2. **Indefinite Hangs:** `None` restarts the failed container via Kubelet, but sibling pods are unaware. Distributed applications like PyTorch DDP hang indefinitely on stale sockets, waiting for a peer that has restarted with an empty state.

To support enterprise AI workloads reliably, LWS must coordinate all group members toward the same restart generation synchronously in place, avoiding K8s scheduling overhead while guaranteeing a clean application-level re-initialization.

---

## 4. Architectural Design (True Group Restart)

### 4.1 The `RestartAllContainers` Primitive

This KEP relies on Kubernetes 1.36's `RestartAllContainersOnContainerExits` feature. By mapping specific container exit codes to the `RestartAllContainers` action via `restartPolicyRules` directly on the application container, the initial workload crash natively forces the Kubelet to restart the entire pod lifecycle (including init containers).

### 4.2 The Credential-Less Restartable Init Container Agent

To block fast-restarting workers from launching before the group is ready, the LWS webhook injects a **restartable init container** (a native sidecar introduced in K8s 1.28) named `lws-restart-agent`.

**Zero API Access:** The agent receives no ServiceAccount token and makes no K8s API calls.
Instead, the LWS Controller acts as the authority and mirrors commands onto each Pod as annotations. The agent reads these via a **Downward API volume** (not using `subPath`, so updates propagate atomically).

```yaml
spec:
  initContainers:
  - name: lws-restart-agent
    image: registry.k8s.io/lws-agent:v0.1.0
    restartPolicy: Always
    volumeMounts:
    - name: lws-restart-state
      mountPath: /var/run/lws
      readOnly: true
    - name: lws-agent-local-state
      mountPath: /var/run/lws-local
    startupProbe:
      exec:
        command: ["/agent", "probe", "startup"]
    readinessProbe:
      exec:
        command: ["/agent", "probe", "readiness"]
    restartPolicyRules:
    - action: RestartAllContainers
      exitCodes:
        operator: In
        values: [88]
```

### 4.3 Coordination via Leader Pod Annotations

All authoritative recovery state is stored durably as annotations on the **Leader Pod**, updated by the controller using optimistic concurrency (`RetryOnConflict`). The controller uses a stable attempt identifier (e.g., `<Pod UID>/<container name>/<restart count>`) to deduplicate simultaneous crashes.

---

## 5. API Design & Schema Modifications

### 5.1 Group Restart Configuration & Workload Triggers

We introduce an explicit configuration to map application exit codes to recovery behaviors.

```go
type InPlaceGroupRestartTrigger struct {
	ContainerName string `json:"containerName"`
	// Exit codes that natively trigger RestartAllContainers.
	// +kubebuilder:validation:MinItems=1
	RecoverableExitCodes []int32 `json:"recoverableExitCodes"`
}

type InPlaceGroupRestartConfig struct {
	Triggers []InPlaceGroupRestartTrigger `json:"triggers"`

	// +kubebuilder:default=5
	MaxAttempts int32 `json:"maxAttempts,omitempty"`

	// +kubebuilder:default="10m"
	Window metav1.Duration `json:"window,omitempty"`

	// Timeout for the barrier to clear before FallbackPolicy is executed.
	// +kubebuilder:default="5m"
	RecoveryTimeout metav1.Duration `json:"recoveryTimeout,omitempty"`
}
```

*Note: The `FallbackPolicy` is implicitly restricted to full group recreation to prevent recursive definitions.*

### 5.2 Initial Startup Policy Deadlock Prevention

**Validation Rule:** The webhook will reject `RestartPolicy: InPlaceGroupRestart` unless `StartupPolicy` is explicitly `LeaderCreated`.

---

## 6. Controller State Machine & Deduplication

### 6.1 The Triggering Pod Handshake

To prevent double-restarts on the initial failure:
1. Workload exits with a recoverable code. Kubelet performs `RestartAllContainers`.
2. The agent restarts. It maintains a `boot-count` in a local `emptyDir` (which survives in-place restarts).
3. The agent sees its `boot-count > 0`, but the Downward API `desired-generation` hasn't changed yet. It remains in `AwaitingGeneration`, failing its `startupProbe`, **without exiting `88` again**.
4. The controller detects the new failure, allocates generation `N`, and updates the Downward API.
5. The already-restarted triggering agent records generation `N` and lifts its barrier.

### 6.2 Controller Phases (Leader Pod)

1. **`Idle`**: Normal operation.
2. **`Quiescing`**: Controller sets `barrier-open=false` on all pods to strip readiness and stop traffic to stale old-generation members.
3. **`SignalingGroup`**: Controller mirrors `desired-generation=N` to all pods.
4. **`WaitingForAcknowledgements`**: 
   - Healthy agents exit `88` and Kubelet restarts them. 
   - Controller watches live PodStatus. When a pod's agent restart-count increments, `PodRestartInPlace=False`, and UID is unchanged, the controller considers generation `N` acknowledged.
5. **`WaitingForReadiness`**: Controller sets `barrier-open=true`. Agents pass `startupProbe` and `readinessProbe`.
6. **`Recovered`**: All pods reach `Ready`. Phase returns to `Idle`.

*If a node dies, replacement pods initialize with `observed-generation=N` directly and do not exit 88, simply waiting at the barrier.*

---

## 7. Edge Cases & Resilience

- **Leader Deletion:** If the Leader Pod is deleted, authoritative state is lost. The controller abandons in-place recovery and recreates the full group.
- **Rollouts & Scaling:** 
  - Scale up/down of *other* groups does not affect recovery.
  - Scale down of the recovering group, or a template revision change affecting the recovering group, immediately cancels in-place recovery and escalates to full recreation.
- **Timeout vs. Budgets:** If the `RecoveryTimeout` is breached during a phase (e.g., due to a slow Kubelet), the recovery escalates to full recreation immediately. `MaxAttempts` specifically limits the total number of distinct crash attempts within the `Window`.

---

## 8. Requirements on Workload Images

This KEP assumes that workload authors (e.g., PyTorch, Ray configurations) emit consistent, documented exit codes for "recoverable" failures. The `InPlaceGroupRestartTrigger` API explicitly pushes this contract onto the user container. Silently re-interpreting all non-zero exit codes as recoverable is strictly avoided.

---

## 9. Operational Risks & Side Effects

1. **Downward API Latency:** Downward API volumes are not push-updated. Kubelet resyncs them based on its sync period (~1 minute default). The `RecoveryTimeout` and general SLA for group restarts are bounded by this Kubelet sync cadence, not by controller reaction time.
2. **Graceful Termination Loss:** `RestartAllContainers` is abrupt. `preStop` hooks are skipped, and `terminationGracePeriodSeconds` is bypassed. Workloads must rely on independent/async checkpointing rather than graceful-exit signals.

---

## 10. Feature Gates & Kubernetes Compatibility

The cluster administrator is responsible for ensuring the `RestartAllContainersOnContainerExits` feature gate is enabled on eligible Kubelets.

**Validation Webhook Checks:**
- Rejects admission if Kubernetes server version `< 1.36`.
- **Runtime Fallback:** A version check does not guarantee the Beta gate is enabled cluster-wide. If a target pod does not expose the expected `PodRestartInPlace` transition or fails to restart within the `RecoveryTimeout`, LWS escalates to full recreation.

---

## 11. Test Plan

E2E validation will test actual agent barriers rather than simulated patches.

```go
ginkgo.It("Should perform a synchronized true in-place group restart", func() {
    // 1. All group Pods initially reach Ready.
    // 2. Fetch live baseline Pod UIDs and restart counts from API.
    
    // 3. Trigger a real workload failure.
    simulateRecoverableCrash(worker0, "pytorch-container")

    // 4. Observe the PodRestartInPlace condition on triggering pod.
    gomega.Eventually(func() bool {
        pod := getLivePod(worker0)
        return hasCondition(pod, "PodRestartInPlace", "True")
    }).Should(gomega.BeTrue())

    // 5. Verify healthy peers exit with 88 and restart natively.
    gomega.Eventually(func() int32 {
        return getLiveAgentRestartCount(worker1)
    }).Should(gomega.BeNumerically(">", initialCount))

    // 6. Verify UIDs and IPs are preserved.
    gomega.Consistently(func() types.UID { return getLivePod(worker1).UID }).Should(gomega.Equal(initialUID))
    
    // 7. Verify applications do not start before controller lifts barrier.
    gomega.Eventually(func() bool {
        return isAppContainerRunning(worker1)
    }).Should(gomega.BeTrue())
})
```
