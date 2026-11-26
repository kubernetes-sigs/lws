---
title: "Canary Rollouts with Argo CD"
linkTitle: "Argo CD Canaries"
weight: 55
description: >
  Configure Argo CD to apply DisaggregatedSet canary changes from Git and show when each requested percentage is ready.
---

Argo CD applies your DisaggregatedSet YAML from Git. **LWS reads `canary.percent` and performs the rollout.** You configure Argo CD to show when LWS has reached the requested percentage; no custom Argo CD image is needed.

{{% alert title="Requires the percentage-canary implementation" color="info" %}}
This guide accompanies [the implementation in #1307](https://github.com/kubernetes-sigs/lws/pull/1307). It requires that controller and its matching CRDs; the documentation alone does not enable canary support.
{{% /alert %}}

Before starting, install an LWS controller and matching CRDs that support `spec.rolloutStrategy.canary.percent`, and have Argo CD running. See [tested Argo CD versions](#argo-cd-versions) below. The percentage applies to replica groups in each role and sub-role, with Ordinal or Hash identities. Your inference router still decides which revision receives each request; the percentage does not configure traffic weights.

## Configure Argo CD

Set up the health check and Application below before changing the canary percentage. DisaggregatedSet does not install this Argo CD configuration for you.

### Report canary health

Stock Argo CD has no built-in DisaggregatedSet health check. It can sync the resource, but without a customization the DisaggregatedSet does not contribute a readiness assessment to Application health. An Application can therefore look Healthy while its DisaggregatedSet is still rolling out.

Save the following as `argocd-canary-health.yaml`. This is a merge patch for the existing `argocd-cm` ConfigMap:

```yaml
data:
  resource.customizations.health.disaggregatedset.x-k8s.io_DisaggregatedSet: |
    local waiting = {status = "Progressing", message = "Waiting for the current DisaggregatedSet status"}
    local generation = obj.metadata and obj.metadata.generation
    if generation == nil or obj.status == nil or obj.status.observedGeneration ~= generation then
        return waiting
    end

    local available = nil
    local progressing = nil
    for _, condition in ipairs(obj.status.conditions or {}) do
        if condition.observedGeneration == generation and condition.status == "True" then
            if condition.type == "Progressing" then
                progressing = condition
            elseif condition.type == "Available" then
                available = condition
            end
        end
    end

    if progressing ~= nil then
        local status = "Progressing"
        if progressing.reason == "CanaryHeld" then
            status = "Healthy"
        end
        return {status = status, message = progressing.message or progressing.reason}
    end
    if available ~= nil then
        return {status = "Healthy", message = available.message or "All roles are ready"}
    end
    return waiting
```

Choose your cluster context explicitly; replace `YOUR_CONTEXT` and, if necessary, the Argo CD namespace:

```sh
kubectl --context YOUR_CONTEXT -n argocd patch configmap argocd-cm \
  --type merge --patch-file argocd-canary-health.yaml
```

Argo CD loads the ConfigMap change automatically. Refresh the Application to re-evaluate its health. The customization only accepts status and conditions observed for the current resource generation:

- `CanaryHeld` becomes Healthy: the requested partial hold has settled. This does not accept the release on your behalf.
- `Available=True` becomes Healthy on completion.
- Missing or stale status, an active rollout, or a canary that cannot yet run remains Progressing, with the controller's explanation when available.

{{% alert title="A healthy canary can have Available=False" color="warning" %}}
During a successful partial hold, `Progressing=True` with reason `CanaryHeld` and **`Available=False`**. These conditions are mutually exclusive: `Available` means the rollout has finished on the desired templates, not just that the application is serving. Do not alert on `Available=False` alone during a planned hold. Check `MinimumReplicasAvailable` for the configured serving-capacity floor, together with application health, error rate, and latency. Replica readiness does not prove that the new version is safe to promote.
{{% /alert %}}

The patch preserves unrelated ConfigMap entries but replaces this kind's existing health customization. If Git or Helm manages `argocd-cm`, put the same entry in that source of truth so the change is not reverted. Review any existing customization before replacing it.

For dashboards or promotion automation, `status.canary` also exposes the observed request and rollout identity directly:

```yaml
status:
  canary:
    percent: 25
    dimension: Groups
    baselineRevisions: [revision-a]
    targetRevision: revision-b
    settled: true
```

`settled` confirms the requested hold is Ready, not that the application passed your checks. The baseline list contains the recorded revisions across slices; it can be empty while the controller is still identifying the baseline. Replica counts stay in `status.roleStatuses`, including sub-role counts. This section is absent when no partial canary is active, including finalization at `100` or omission. Check `status.observedGeneration` against `metadata.generation`. With External scaling, also compare each reported `desiredReplicas` with its RoleScaler's `spec.replicas`: autoscaling does not change the parent generation.

### Connect your deployment repository

The Application tells Argo CD where your deployment YAML lives and which cluster receives it. Keep your existing Application if you already have one. Otherwise, save an Application such as this as `inference-application.yaml`:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: inference
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/YOUR_ORG/inference-config.git
    targetRevision: production
    path: inference/prod
  destination:
    server: https://kubernetes.default.svc
    namespace: inference
```

Replace the repository, branch, path, project, and destination with your deployment's values. `targetRevision: production` is the Git branch; `path: inference/prod` is the directory containing your DisaggregatedSet manifest. This example uses the cluster where Argo CD runs and assumes the `inference` namespace exists.

Create the Application in the cluster running Argo CD:

```sh
kubectl --context YOUR_CONTEXT apply -f inference-application.yaml
```

Sync it from the Argo CD UI or with `argocd app sync inference`. This example uses manual sync. If your Application already enables automatic sync, Argo CD can apply the Git changes automatically instead.

Argo CD applies `canary.percent` just like the other fields in your YAML. The LWS controller then adjusts the replica groups and writes their status. The health check above lets Argo CD show **Progressing** while LWS works and **Healthy** when the requested canary percentage is ready.

**Synced and Healthy answer different questions.** Synced means the live configuration matches Git. Healthy means LWS reports that the requested groups are Ready. After a percentage change, the Application can be Synced while it is still Progressing.

### Argo CD versions

Use the latest patch of a maintained Argo CD release. Argo CD [maintains its three most recent minor versions](https://github.com/argoproj/argo-cd/blob/v3.6.1/SECURITY.md#supported-versions): as of November 6, 2026, these are **3.4, 3.5, and 3.6**.

**The earlier configuration passed on Argo CD 3.2.0 and the later releases listed below. The updated examples still need live validation.**

The matrix below was tested with the earlier, flat `canaryPercent` API, using official binaries and real Argo CD servers. The examples on this page now use the nested `canary.percent` API; that version matrix has not yet been rerun with the updated examples. The last column records whether Argo CD preserved a percentage managed by external automation and reported Synced. Results apply to the exact releases and earlier examples tested:

| Tested release | Git sync, health and full promotion | Set Canary Percentage | External automation |
| --- | --- | --- | --- |
| 3.0.0 | Passed | Unavailable | False OutOfSync |
| 3.1.0 | Passed | Passed | False OutOfSync |
| 3.2.0 | Passed | Passed | Passed |
| 3.3.0 | Passed | Passed | Passed |
| 3.4.1 | Passed | Passed | Passed |
| 3.5.0 | Passed | Passed | Passed |
| 3.6.0 | Passed | Passed | Passed |

Argo CD identifies **3.4.1** as [the first release in the 3.4 series](https://github.com/argoproj/argo-cd/releases/tag/v3.4.1).

**Why the percentage action fails on 3.0.0:** it cannot pass the user's chosen number into a custom action. The setter rejects the missing value without changing the workload. [Argo CD 3.1 added action parameters](https://github.com/argoproj/argo-cd/releases/tag/v3.1.0). On 3.0.0, change the percentage in Git, or use **Promote All Roles**, which always sets `100` and needs no input.

**Why the earlier external-automation example showed OutOfSync on 3.0.0 and 3.1.0:** excluding the old `canaryPercent` field from the comparison left an empty `rolloutStrategy` object on one side and `null` on the other. Sync succeeded and preserved the live percentage, but Argo CD still reported a difference. Automation waiting for Synced could get stuck. Use Git-owned promotion on these releases or validate the updated ownership example before relying on it. The five later releases in the table passed the earlier example.

Validation included **188 passing local health/action checks** and live Git sync, configuration reload and action tests on each release. The servers ran on Kubernetes **1.32.0** for Argo CD 3.0.0–3.2.0 and **1.34.0** for the remaining releases. We supplied simulated DisaggregatedSet status; no LWS controller ran. Current [LWS requires Kubernetes 1.34 or newer](../../../installation/#before-you-begin), outside the published test ranges of Argo CD 3.0.0–3.2.0. Passing the configuration checks on those older releases does not establish a supported setup for running LWS.

## Deploy gradually by changing the percentage in Git

Start with a healthy revision A. Here, **promote** means allowing more groups to run the new version B. Each step changes the DisaggregatedSet manifest in your deployment repository. In one Git commit, change the intended role templates to revision B **and** set the initial percentage. For a zero-progress starting point, add this to the existing DisaggregatedSet manifest while making the template changes:

```yaml
spec:
  rolloutStrategy:
    canary:
      percent: 0
      dimension: Groups
```

This is a manifest fragment, not a replacement for the existing `spec`. Keep the roles and other configuration. Committing the template first and the percentage later can allow the rollout to advance before the hold is applied.

Commit and push the change, then sync the Application. Once Argo CD shows Healthy, change `canary.percent` to `25`, commit and push again, and sync again. Repeat for `50` and then `100`, checking the workload before each increase. With four Prefill and eight Decode groups per slice, the settled counts are:

| Percentage | Old Prefill / Decode | New Prefill / Decode |
| --- | --- | --- |
| `0` | 4 / 8 | 0 / 0 |
| `25` | 3 / 6 | 1 / 2 |
| `50` | 2 / 4 | 2 / 4 |
| `100` | 0 / 0 | 4 / 8 |

Argo CD does not choose the next percentage or judge the model's answers: you or your release automation make that decision. Healthy means the requested groups are Ready. Readiness and rollout budgets still govern every transition; temporary surge groups are not shown in the table. If autoscaling changes demand, LWS recalculates both shares.

**100% finalizes the canary, like omitting the percentage.** B serves the full desired workload, and A's LWS objects can be cleaned up. During a partial canary, percentage decreases are rejected. Keep the percentage unchanged while evaluating B; use a Git template revert for rollback, before or after completion.

After promoting to 100%, wait for `Available=True` with reason `AllRolesReady` for that generation before starting another canary. Both a partial hold and a finalized rollout appear Healthy with the configuration above; their condition reasons distinguish them. No second edit removing the percentage is required to accept B as the next baseline. Set the next starting percentage together with the new templates.

Once B finishes, a new C canary uses B as its baseline. Keeping `100` configured does not automatically pause the next template update below 100%.

{{< canvas-animation data="/animations/canary-promotion.json" title="Promote a canary: 0 → 25 → 50 → 100%" >}}
Four Prefill and eight Decode groups move from A to B. Each role has `maxSurge: 1` and `maxUnavailable: 0`: new groups become Ready before old groups leave. At 100%, the rollout finishes and A's LWS objects can be cleaned up. The marker shows the requested percentage; the bars show Ready groups on B. Timing is illustrative.
{{< /canvas-animation >}}

Initial creation without an old revision starts the full workload, even with `0`. A positive partial group canary (`1`–`99`) needs at least two groups in each active logical role. If one role has only one group, the whole slice stays on A and continues scaling; its status explains why it cannot yet run a canary. A Ready hold confirms replica readiness, not the quality of the model's answers or its latency: your checks or analysis must decide whether to promote.

A fresh `External` role starts with one group until its autoscaler or operator updates the RoleScaler. This does not freeze peer scaling. For multi-slice Static deployments with small roles, choose whole-slice canaries instead.

### Canary complete slices

The same Argo CD health check and percentage actions work with `canary.dimension`. Set the dimension in Git together with the initial hold and new templates:

```yaml
spec:
  slices: 8
  rolloutStrategy:
    canary:
      percent: 25
      dimension: Slices
```

For eight slices of 1 Prefill + 1 Decode, two complete slices roll to B and six remain on A. Increasing the percentage to 50 adds two B slices; returning to 25 is rejected. B → C updates the selected slices while the others remain on A. Each transition still follows the slice's rollout budgets and readiness checks.

Choose `Groups` (the default) or `Slices` in Git. There is no automatic dimension choice or hidden dimension annotation. Role scaling does not change the configured dimension.

Dimension changes are rejected during partial canaries, as are slice-count changes in `Slices` mode. Finish at `100` or remove the percentage before changing them. This does not add slice autoscaling, and the existing restriction on External scaling with multiple slices still applies.

## Replace the canary while keeping its baseline

Suppose A is the baseline and B is held at 50%. You can change the templates to C while keeping `canary.percent: 50`. A keeps its half; B rolls to C within the other half. The B-to-C rollout does not scale or recreate A, so its existing LWS and Pod UIDs remain unchanged while the percentage and demand stay fixed.

For four Prefill / eight Decode groups, the settled counts are:

| State | Baseline A | Canary B | Canary C |
| --- | --- | --- | --- |
| B held at 50% | 2 / 4 | 2 / 4 | 0 / 0 |
| C held at 50% | 2 / 4 | 0 / 0 | 2 / 4 |

During the replacement, B and C temporarily share the canary half, with extra groups allowed by `maxSurge`. C must supply the required Ready capacity before B drains. Budgets are calculated from the full desired workload, and the Ready baseline contributes to its availability floor. The controller does not treat A and B as interchangeable old capacity and consume A to make room for C.

{{< canvas-animation data="/animations/canary-replacement.json" title="Replace B with C; keep baseline A" >}}
A keeps the same two Prefill and four Decode groups while B rolls to C. The budgets apply to the full desired workload: four Prefill and eight Decode. Hover or tap a group for its state. Timing and deletion order are illustrative.
{{< /canvas-animation >}}

The [companion canary harness in #1309](https://github.com/kubernetes-sigs/lws/pull/1309) checks A's workload and Pod identities while B is replaced by C, for ordinary roles and sub-roles with Ordinal and Hash identities. Its recordings are delivered separately from this guide.

## Roll back through Git

Promotion moves forward: `25 → 50` is allowed, but `50 → 25` and `25 → 0` are rejected. Keep the percentage unchanged to pause promotion. Starting at `0` holds B before its first groups are created; it is not an abort operation after promotion starts.

{{% alert title="Restore the templates, not just the percentage" color="danger" %}}
To roll back, **restore A's role templates in Git and remove `canary.percent` in the same commit**, then sync Argo CD. Removing the percentage without restoring A's templates would finish deploying B.

LWS rolls back using readiness and availability budgets. If A's LWS objects still exist, they are reused; otherwise LWS recreates A from the restored templates. Wait for completion before starting another canary. For the next release, set its starting percentage together with its new templates.
{{% /alert %}}

## Change demand during a canary

**During a partial canary, only `Immediate` applies demand changes in its direction.** Configure this on the DisaggregatedSet, not in Argo CD:

```yaml
spec:
  scalingPolicy:
    scaleUpDuringRollout: Immediate
    scaleDownDuringRollout: Immediate
```

| Scale up | Scale down | While the canary is held |
| --- | --- | --- |
| Deferred | Deferred | Queue both directions; keep the last accepted counts. |
| Immediate | Deferred | Apply increases; queue decreases. |
| Deferred | Immediate | Queue increases; apply decreases. |
| Immediate | Immediate | Apply both directions. |

With Immediate, at 25%, doubling demand from four Prefill / eight Decode to eight / sixteen gives six old + two new Prefill and twelve old + four new Decode. Reducing demand back to four / eight restores three old + one new Prefill and six old + two new Decode.

Deferred is the default. It keeps the last accepted count, not temporary surge replicas. A request from 4 to 8 therefore stays at 3 A + 1 B. Promoting to 50% still gives 2 A + 2 B, and B → C can still roll within that size. The saved counts survive controller restarts. Switching a direction to Immediate applies only the latest demand, not every queued autoscaler value. A zero request also waits under Deferred scale-down.

Readiness, `maxSurge`, `maxUnavailable`, and safe deletion still apply. At `100` or omission, ordinary rollout policies resume and the latest demand can be applied. Outside a partial canary, Deferred scale-up keeps its normal readiness-paced behavior. Status still shows the requested demand; a step with queued demand is not reported as settled `CanaryHeld`.

{{< canvas-animation data="/animations/canary-scaling.json" title="Autoscale both revisions at 25%" >}}
With both scaling directions set to Immediate, double desired capacity from four Prefill / eight Decode to eight / sixteen, then halve it again. Both revisions resize to preserve the 25% split. A higher availability floor takes time to fill; existing excess after a reduction drains safely. Timing and intermediate changes are illustrative.
{{< /canvas-animation >}}

LWS chooses the **oldest revision with surviving groups** as the baseline and records that choice across template updates and controller restarts. Its role/sub-role templates must match the requested layout, but its replica counts may reach zero. If a required template is missing or the baseline identity is ambiguous, LWS explains the problem through `Progressing` rather than silently treating a younger canary as the baseline. This selection does not determine whether the revision passed your application checks.

With Immediate in the corresponding direction, an existing role can scale to zero and grow back from its saved templates. During a partial canary, whole-set zero retains both baseline and target LWS objects; resuming restores the split. In slice mode, role resizing keeps the same selected slices. Finalizing at 100% or by omitting the percentage allows the baseline to be retired; lowering the percentage afterward cannot recreate its deleted templates. See [Rollout Strategy](../rollout-strategy/#one-percentage-for-all-roles) for these limits and small-role rounding.

The [companion harness in #1309](https://github.com/kubernetes-sigs/lws/pull/1309) covers Ordinal and Hash groups, ordinary roles and sub-roles, both scaling policies, and a Static-to-External handoff followed by real scaler updates. Recorded traces require that separate contribution; the animations above illustrate expected behavior and are included with this guide.

## Optional manual actions

Git commits are sufficient for promotion. To also offer **Set Canary Percentage** and **Promote All Roles** in Argo CD's resource menu, save this second merge patch as `argocd-canary-actions.yaml`:

```yaml
data:
  resource.customizations.actions.disaggregatedset.x-k8s.io_DisaggregatedSet: |
    discovery.lua: |
      local roles = obj.spec and obj.spec.roles or {}
      local strategy = obj.spec and obj.spec.rolloutStrategy
      local percent = strategy and strategy.canary and strategy.canary.percent
      return {
          ["promote-full"] = {
              ["disabled"] = #roles == 0 or percent == nil or percent == 100,
              ["displayName"] = "Promote All Roles",
          },
          ["set-canary-percent"] = {
              ["disabled"] = #roles == 0,
              ["displayName"] = "Set Canary Percentage",
              ["params"] = {{["name"] = "percent"}},
          },
      }
    definitions:
      - name: set-canary-percent
        action.lua: |
          local params = actionParams or {}
          local percent = tonumber(params.percent)
          if percent == nil or percent < 0 or percent > 100 or percent % 1 ~= 0 then
              error("percent must be an integer between 0 and 100", 0)
          end
          if obj.spec == nil or obj.spec.roles == nil or #obj.spec.roles == 0 then
              error("a DisaggregatedSet must contain roles", 0)
          end
          obj.spec.rolloutStrategy = obj.spec.rolloutStrategy or {}
          obj.spec.rolloutStrategy.canary = obj.spec.rolloutStrategy.canary or {}
          local current = obj.spec.rolloutStrategy.canary.percent
          if current ~= nil and current < 100 and percent < current then
              error("cannot decrease percent; roll back by restoring templates in Git", 0)
          end
          obj.spec.rolloutStrategy.canary.percent = percent
          return obj
      - name: promote-full
        action.lua: |
          if obj.spec and obj.spec.rolloutStrategy and obj.spec.rolloutStrategy.canary and obj.spec.rolloutStrategy.canary.percent ~= nil then
              obj.spec.rolloutStrategy.canary.percent = 100
          end
          return obj
```

```sh
kubectl --context YOUR_CONTEXT -n argocd patch configmap argocd-cm \
  --type merge --patch-file argocd-canary-actions.yaml
```

The first action asks for `percent`, an integer from 0 to 100; it may advance a partial canary but cannot decrease its percentage. The second sets an existing percentage to 100, completing and finalizing the rollout. Use a Git template revert for rollback. Neither action bypasses readiness or availability budgets. Users still need Argo CD permission to run the respective resource action. As with the health patch, this replaces the action customization for this kind, not other ConfigMap entries.

Use the Argo CD resource menu for the parameterized action. In the tested releases, the ordinary `argocd app actions run` command has no parameter flag; the API also supports parameters from 3.1 onward.

{{% alert title="Choose one owner for promotion" color="warning" %}}
These actions change the live DisaggregatedSet, **not Git**. A later sync or automatic self-heal can restore the percentage from Git. Prefer Git-owned progression. Do not mix manual actions, Git updates, and another promotion controller without explicitly assigning ownership of the percentage.
{{% /alert %}}

## Optional external automation

For Git-owned promotion, Kargo can write the template change and initial hold to an environment branch, ask Argo CD to sync, and then write later percentage changes after verification. Git remains the source of truth for both templates and percentage, so no ignore rule is needed. Serialize promotions that share an Application or environment branch. [Robert Shaw's DisaggregatedSet demo](https://github.com/robertgshaw2-redhat/disaggset-argo-demo) contains one Kargo-based workflow; its analysis and routing are separate from this LWS feature.

Argo Rollouts is separate from Argo CD. Automated pauses and analysis require an external integration, such as explicit promotion Jobs, that updates the percentage and waits for the corresponding current-generation LWS status. **No such integration is installed by this guide or shipped with this feature.** A driver Rollout's health alone is not proof that the DisaggregatedSet reached the requested hold. Verification must check the intended release, requested percentage, current desired counts, and Ready groups before approving promotion.

For `External` roles, read the current RoleScaler `spec.replicas` when checking desired counts. Updating a RoleScaler does not change the DisaggregatedSet generation, so matching `observedGeneration` alone does not prove that status reflects the latest scaling request.

This is not native DisaggregatedSet `workloadRef` support. Rollouts `setWeight` does not change `canary.percent`, and the percentage does not configure a traffic router. The integration must also define failure and rollback behavior; a failed analysis does not inherently roll a DisaggregatedSet back.

If an external controller owns the percentage while Argo CD owns the templates, one option is to tell Argo CD to preserve that field. Check the [version results](#argo-cd-versions) first: the earlier flat-field example produced a false OutOfSync result on 3.0.0 and 3.1.0, and this nested-field example has not yet been rerun against those versions. The following is an **Application fragment** for that ownership arrangement. Replace the resource name and namespace:

```yaml
metadata:
  annotations:
    argocd.argoproj.io/compare-options: ServerSideDiff=true
spec:
  syncPolicy:
    syncOptions:
      - ServerSideApply=true
      - RespectIgnoreDifferences=true
  ignoreDifferences:
    - group: disaggregatedset.x-k8s.io
      kind: DisaggregatedSet
      name: canary
      namespace: inference
      jsonPointers:
        - /spec/rolloutStrategy/canary/percent
```

First let Git sync the new templates and their initial percentage hold. **Only then** enable the ignore rule and hand progression to the external controller. Otherwise Git may not be able to establish the initial hold on an existing resource.

`RespectIgnoreDifferences` preserves the controller-owned percentage during sync, including automatic sync or self-heal if already enabled, rather than merely hiding its diff. `ServerSideDiff` uses a server-side dry-run comparison; mutating-webhook changes are excluded unless separately enabled in Argo CD. Ignore only the percentage, not the roles or templates. Keep the template and role layout fixed during an active automated sequence unless the integration explicitly handles concurrent changes; these settings are not a lock.

For a purely Git-driven canary, do not add this ignore rule: Git should continue to own and apply every percentage change.

Another option is to omit the percentage from Git entirely and give it to a separate server-side-apply field manager. This requires a deliberate handoff: removing a field previously owned by Git can remove the live hold. Establish the initial hold before a template rollout, verify field ownership, and ensure delayed promotion work cannot change a different release.
