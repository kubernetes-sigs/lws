标题可以用：

Support Explicit Subgroup Placement with Node Label Constraints

Issue 内容可以这样写：

```md
**What would you like to be added**:

Add support for `subGroupPlacement` in `LeaderWorkerSet` to allow users to explicitly define subgroup membership and constrain each subgroup to nodes with specific labels.

The proposed API would allow users to:
- explicitly assign `workerIndexes` to each subgroup
- specify `matchLabels` for each subgroup
- have the pod webhook translate those `matchLabels` into required `NodeAffinity` for the corresponding worker pods

A sample configuration would look like:

```yaml
subGroupPolicy:
  subGroupPolicyType: LeaderExcluded
  subGroupPlacement:
    - workerIndexes: [1, 2]
      matchLabels:
        remote: schedule_zone
    - workerIndexes: [3]
      matchLabels:
        local: schedule_zone
```

In this example, workers `1` and `2` are constrained to nodes labeled `remote=schedule_zone`, while worker `3` is constrained to nodes labeled `local=schedule_zone`.

**Why is this needed**:

Today, subgroup-level exclusive topology provides topology-based co-location and isolation, but it does not allow users to explicitly control which subgroup should be placed onto which class of nodes.

This is insufficient for workloads that require:
- explicit mapping of subgroup members to distinct node pools or node classes
- deterministic placement of different workers onto nodes with different hardware, locality, or scheduling labels
- subgroup definitions based on workload semantics rather than only topology exclusivity

`subGroupPlacement` addresses a different need from subgroup-level exclusive topology:
- exclusive topology ensures pods in the same subgroup land in the same topology domain and prevents pods with different `subgroup-key` values from sharing that domain
- `subGroupPlacement` explicitly defines subgroup membership and constrains each subgroup to nodes matching user-specified labels

In short, exclusive topology provides subgroup co-location and isolation, while `subGroupPlacement` provides explicit subgroup-to-node placement mapping.

**Completion requirements**:

This enhancement requires the following artifacts:

- [ ] Design doc
- [ ] API change
- [ ] Docs update

The artifacts should be linked in subsequent comments.
```

如果你想让标题更偏 Kubernetes enhancement 风格，也可以用：

1. Add `subGroupPlacement` for Explicit Subgroup-to-Node Placement in LeaderWorkerSet
2. Support Explicit Worker Placement per Subgroup in LeaderWorkerSet
3. Add `subGroupPlacement` API for Label-Based Subgroup Scheduling

如果你要，我可以继续帮你把这份 issue 再压缩成更像 maintainer 风格的简洁版。
