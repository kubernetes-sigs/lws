# DisaggregatedSet regression harness

A bounded, isolated regression lab. It runs the **official LWS v1.0.0 image**,
creates its own kind cluster and private kubeconfig, runs real Pods, and removes
only its owned cluster on success, failure or interruption. No existing cluster
or default Kubernetes context is used. The read-only planner observer is built
against the pinned release source; the controller itself is not rebuilt.

The scenario driver is **native Go**. All small and large cases, readiness
controls, assertions, and evidence recording run without Python. A small shell
launcher owns the kind lifecycle. Previously recorded examples remain historical
evidence, not results from a new run of this driver.

The v1.0.0 baseline is retained deliberately so the recorded examples can be
reproduced. First-group `BootstrapSurge` checks require the corresponding
Kubernetes Event. This is not a claim that the latest LWS release or every
possible interleaving has been tested.

## Requirements and commands

Install Docker (Colima is fine), kind, kubectl, Go compatible with the checkout
and selected release, jq, Git, Bash, curl, shasum and standard Unix tools. Give Docker roughly
8 GiB RAM for these demonstrations. Large cases use up to 102 requested workload
Pods, plus terminating and system Pods. Slots are synthetic resources, not GPUs.

From the repository root:

```sh
git fetch https://github.com/kubernetes-sigs/lws.git refs/tags/v1.0.0:refs/tags/v1.0.0
./test/disaggregatedset_harness/run.sh --list
./test/disaggregatedset_harness/run.sh large-all --dry-run  # no cluster access
./test/disaggregatedset_harness/run.sh all --auto           # 26 small runs
./test/disaggregatedset_harness/run.sh all --include-large --auto  # 28 runs
./test/disaggregatedset_harness/run.sh large-all --auto     # 20 large runs
./test/disaggregatedset_harness/run.sh autoscaler-handoff --auto
./test/disaggregatedset_harness/run.sh large-scale-down --identity hash --rollout-chain abc --auto
```

For selection checks without the shell launcher, use
`go run ./test/disaggregatedset_harness/cmd list` or
`go run ./test/disaggregatedset_harness/cmd dry-run large-all`. Direct `run` needs the private
runtime proof created by the launcher; an arbitrary kubeconfig is refused.

`kind` is detected on PATH. `KIND` can select another executable;
`KIND_NODE_IMAGE` and `HARNESS_MAX_PODS` are configurable. Automatic mode runs
all selected cases even if one fails; `--fail-fast` stops early. Failures and
incomplete matrices produce a nonzero exit status.

`LWS_HARNESS_BASELINE=/absolute/path/baseline.json` selects another immutable
controller baseline. Copy the existing JSON shape: a full source `commit`, and
optionally a release tag, image digest, manifest URL and SHA-256. Source-only
baselines build the controller; release baselines verify the published image
and manifests. This changes what is tested, not the expected scenario contract.
The default remains v1.0.0. These historical cases **do not test the newer
`scaleUpDuringRollout` policy**; the `Immediate` label in `large-scale-up` is
the original `scaleDownDuringRollout` setting, not a scale-up policy comparison.
The observer does follow both policies when the selected source supports them.
For v1.0.0, the launcher explicitly selects its legacy policy adapter; a build
failure never silently switches the observer to different planner semantics.

Use `--interactive` for Enter-to-trigger checkpoints. Press `a` to switch the
remaining run to automatic mode or `q` to stop. Kubernetes waits remain automatic.
Checkpoints use the invoking terminal; the harness does not launch a desktop
terminal or capture k9s. Execution and evidence capture do not need Node.js,
Python, a browser, or visualization tools.

## Small matrix

Each family runs with **Ordinal and Hash**. Scale-down covers both policies;
outage covers integer and percentage budgets.

| Family | What it checks | Runs |
| --- | --- | ---: |
| `ordinary` | 2 Prefill/4 Decode, ordinary asymmetric update and replica-only resizing without a revision change. | 2 |
| `scale-up` | During a mixed rollout, 4→6 per role. Grow the target; retain needed Ready and loading capacity. | 2 |
| `scale-down` | During a mixed rollout, 4→2 per role. Deferred versus Immediate with safe old drains. | 4 |
| `autoscaler-handoff` | Static→External with 3 Prefill/2 Decode two-Pod groups; 15-second delayed scaler write, then `/scale` 3→4→2. Original Pod UIDs must survive the handoff. | 2 |
| `rename` | Rename both roles at 2/2 under tight capacity; share released slots. | 2 |
| `rename-outage` | Rename at 2/3 with zero surge and 100% unavailable; explicit full downtime, then independent readiness release. | 2 |
| `restart-rollback` | A→B→C interruption, controller restart/cold cache and rollback to B. | 2 |
| `subroles` | Two-Pod groups, shared physical LWS, child relabel/shrink/restart/grow/rollout. | 2 |
| `native-restart` | Worker and leader crashes recreate only their affected group. | 2 |
| `outage` | 1/2 in three slots, zero surge, explicit allowed downtime without losing another role's protection. | 4 |
| `emergency` | Interrupted history, one bootstrap group, real ≥60-second Unschedulable timeout and bounded availability fallback. | 2 |
| `large-rename` | Optional asymmetric 9/4→8/3 rename with integer rounding and shared capacity. | +2 |

The emergency case seeds interrupted revision metadata while the controller is
paused. It does not forge Pod readiness or scheduler timeout ages. The autoscaler
case uses the real `/scale` endpoint, not a metrics-driven HPA or KEDA.

## Large matrix

Start at **37 Prefill / 19 Decode**, one Pod per group. Every family covers
Ordinal/Hash and A→B/A→B→C. C interrupts B while substantial A and B remain Ready.

| Family | Change | Runs |
| --- | --- | ---: |
| `large-scale-up` | Immediate, 37/19→55/29, preserve old Ready and scheduled target work. | 4 |
| `large-scale-down` | 37/19→28/14, both budgets 10%, Deferred and Immediate. Keep needed old work visible. | 8 |
| `large-ordinary` | Roll templates without changing desired; release readiness in batches. | 4 |
| `large-subroles` | Shared parents with four independent children; Prefill 25/12→21/16 and Decode 13/6→10/9, then rollout. | 4 |

Large virtual-role tests recognize a bounded first-group bootstrap exception only
when the matching Kubernetes Event authorizes it. They do not enlarge the normal
surge budget permanently. The gentler scale-down can legitimately look similar
under Deferred and Immediate because both retain capacity needed for availability.

## Evidence and safety

The captured data separates requested groups, whole-group Ready capacity and
independent planner bounds, including availability floors, surge ceilings and
pending allowance. A Ready worker is not a Ready group without its leader.
Observer previews are not atomic snapshots or controller decision logs.

Artifacts remain under ignored `test/disaggregatedset_harness/.state/`. Before mutation, the Go
driver checks the node identity, kind labels, read-only ownership-token mount,
token, private context, and that the kubeconfig endpoint is the owned node's
published API port. The launcher rechecks ownership before deletion. Ctrl-C
and normal termination clean up; SIGKILL or losing Docker cannot guarantee that.
An ownership mismatch refuses cleanup rather than deleting an unrelated cluster.

Each native run records:

- `results.json` and each case's `result.json`: actual outcomes and checkpoints.
- `film-recording/metadata.json`: capture schema version, controller commit,
  producer, sampling interval and coverage caveats.
- `film-recording/actions.jsonl`: requested values, real operations, notes and
  passing/failing assertions. An assertion's text is its failure condition.
- `film-recording/snapshots.jsonl`: independent one-second API acquisition
  windows, including Events, with explicit read errors and no invented state.
- `<case>-<identity>-<policy>/observer.jsonl`: independent read-only planner
  bounds from the existing Go observer built against the selected baseline.
- Per-case samples and failure diagnostics, plus the launcher's final resources,
  controller log and cleanup result.

JSONL files contain one JSON object per line. Records carry a case ID and
timestamps so consumers can correlate requested operations with later API
observations. Action records distinguish operation start and completion;
snapshot records retain their acquisition start/end and read errors. Missing
observations must not be interpreted as an empty cluster.

For a quick outcome summary, use the run directory printed by the launcher:

```sh
jq '.[] | {case, identity, policy, passed, error}' /path/to/owned-run/results.json
```

Portable JSON normalization and browser visualization are separate, optional
integrations. A normalizer can consume the raw files above without rerunning the
cluster, join only observer samples recorded before an API read, and retain
unknown counts and explicit failures. Neither the normalizer nor the player is
included or required by this harness.

Capture does not claim continuous LIST/watch or controller log coverage. Raw
snapshots and command diagnostics can contain private data; review them before
sharing. Never publish a run's kubeconfig or ownership token.

## Read-only planner observer

`observer/testdata/` contains ordinary Go source files. Go excludes `testdata`
from normal package discovery, keeping these diagnostic helpers out of production
builds. The launcher uses a small Go `-overlay` mapping to build the standalone
observer against the pinned source archive. It gives the diagnostics access to
private planner helpers during that build only, without copying files into the
archive, changing the controller, or adding production source under
`pkg/controllers`.

The observer uses a Reader-only Kubernetes client. Its bounds are independent
observations, not controller write authorization. Current sources propagate
both scaling policies. Sources predating independent scale-up, such as v1.0.0,
explicitly select `harness_legacy_scale_up`; inconsistent declarations or build
errors fail visibly rather than silently switching semantics.

Offline checks:

```sh
go test -race ./test/disaggregatedset_harness/...
go vet ./test/disaggregatedset_harness/...
bash -n test/disaggregatedset_harness/run.sh test/disaggregatedset_harness/observer/prepare.sh
```

The normal `go test` command above also builds the observer and runs its
private-planner diagnostic tests through a temporary overlay against this
checkout. No separate test command or source edits are needed.

The Go package exposes selection and suite execution, with injectable process
execution and clocks. Scenario operations abort immediately on failed checks;
the public suite API returns errors. Offline tests cover the complete matrix,
whole-group readiness, assignment publication, midpoint checks, bootstrap event
credit, physical completion, ownership, real command arguments, cancellation,
emergency restoration and evidence schema. They do not replace a live kind run.
