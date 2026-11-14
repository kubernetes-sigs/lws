# DisaggregatedSet regression harness

A bounded, isolated regression lab. By default, it builds the controller and
read-only planner observer against an immutable archive of the checkout's
**Git HEAD**. It creates its own kind cluster and private kubeconfig, runs real
Pods, and removes only its owned cluster on success, failure or interruption.
No existing cluster or default Kubernetes context is used.

The scenario driver is **native Go**. All small and large cases, readiness
controls, assertions, and evidence recording run without Python. A small shell
launcher owns the kind lifecycle. Previously recorded examples remain historical
evidence, not results from a new run of this driver.

First-group `BootstrapSurge` checks require the corresponding Kubernetes Event.
The checks cover the selected scenarios, not every possible interleaving.

## Canary support is work in progress

The canary scenarios require the controller implementation in [#1307](https://github.com/kubernetes-sigs/lws/pull/1307). This harness change targets `main` separately; it does not include that implementation. Until #1307 is merged, the canary observer extension cannot compile against `main`, so `TestObserverOverlayAgainstRepository` is expected to fail. The scenario-driver tests and recorded trace assets can be checked independently.

To run the canary scenarios now, use a local integration commit containing both #1307 and this harness change. The launcher builds the checked-out **Git HEAD**, including the controller, CRDs and observer; it has no separate `--ref` or controller-source option. Commit the combined changes before running. Once #1307 is merged, the ordinary commands below work from an updated checkout containing this harness.

## Requirements and commands

Install Docker (Colima is fine), kind, kubectl, Go compatible with the checkout,
jq, Git, Bash and standard Unix tools. Give Docker roughly 8 GiB RAM for these demonstrations.
Large cases use up to 102 requested workload
Pods, plus terminating and system Pods. Slots are synthetic resources, not GPUs.

From the repository root:

```sh
make disaggset-test-harness-small  # 38 small scenarios, real kind cluster
make disaggset-test-harness-large  # 20 large scenarios, real kind cluster

./test/disaggregatedset_harness/run.sh --list
./test/disaggregatedset_harness/run.sh large-all --dry-run  # no cluster access
./test/disaggregatedset_harness/run.sh all --include-large --auto  # 40 runs
./test/disaggregatedset_harness/run.sh canary canary-subroles --auto  # 8 canary runs
./test/disaggregatedset_harness/run.sh canary-chain canary-chain-subroles --auto  # 4 fixed-baseline A→B→C runs
./test/disaggregatedset_harness/run.sh autoscaler-handoff --auto
./test/disaggregatedset_harness/run.sh large-scale-down --identity hash --rollout-chain abc --auto
```

The Make targets run `run.sh all --auto` and `run.sh large-all --auto`,
respectively. They are opt-in; ordinary `make test` does not start a kind cluster.

Commit local controller changes before running: the HEAD archive deliberately
excludes uncommitted changes. In CI, check out the commit to test and run either
Make target. Branch, detached HEAD, tag and pull-request merge checkouts all use
that exact commit. A tagged checkout must contain the harness; there is no
separate controller source or published-image selection.

For selection checks without the shell launcher, use
`go run ./test/disaggregatedset_harness/cmd list` or
`go run ./test/disaggregatedset_harness/cmd dry-run large-all`. Direct `run` needs the private
runtime proof created by the launcher; an arbitrary kubeconfig is refused.

`kind` is detected on PATH. `KIND` can select another executable;
`KIND_NODE_IMAGE` and `HARNESS_MAX_PODS` are configurable. Automatic mode runs
all selected cases even if one fails; `--fail-fast` stops early. Failures and
incomplete matrices produce a nonzero exit status.

The `Immediate` label in `large-scale-up` is the `scaleDownDuringRollout` setting,
not a comparison of the independent `scaleUpDuringRollout` policies. The observer
follows both independent policies in the checked-out controller.

Use `--interactive` for Enter-to-trigger checkpoints. Press `a` to switch the
remaining run to automatic mode or `q` to stop. Kubernetes waits remain automatic.
Checkpoints use the invoking terminal; the harness does not launch a desktop
terminal. Execution and evidence capture do not need Node.js, Python, a browser,
or visualization tools.

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
| `canary` | 4 Prefill/8 Decode: 25→50→25→0→25, desired 4/8→8/16→4/8, then 100%. Both scaling policies; retain target LWS UIDs at 0%. | 4 |
| `canary-subroles` | The same forward/reverse/resizing sequence with Prefill and Decode sharing a physical LWS. Both identities and policies. | 4 |
| `canary-chain` | Hold A at 50%, then replace B with C in the other half. A's spec and Pod UIDs must stay unchanged. Ordinal and Hash. | 2 |
| `canary-chain-subroles` | The same A→B→C sequence with shared physical LWSs; also preserve A's child assignments. | 2 |
| `large-rename` | Optional asymmetric 9/4→8/3 rename with integer rounding and shared capacity. | +2 |

The emergency case seeds interrupted revision metadata while the controller is
paused. It does not forge Pod readiness or scheduler timeout ages. The autoscaler
case uses the real `/scale` endpoint, not a metrics-driven HPA or KEDA.

The canary cases use `maxSurge=1` and `maxUnavailable=0` for each logical role.
Both A and B have real application-readiness gates. On a percentage decrease,
replacement A groups first remain scheduled but unready, proving that needed
Ready B capacity is retained until the gates are opened. At 0%, B has no Pods but
its original LWS objects remain, and returning to 25% reuses them. Percentage and
replica-only changes must not create another revision.

Before resizing, the reversible canary cases switch both roles (or subroles) to External
scaling. The handoff must preserve all Pod/LWS identities and seed scalers from
the existing 4/8 target. The subsequent 8/16 and 4/8 requests use the real `/scale`
subresources, and a completed hold requires current scaler observations.

The four `canary-chain` cases instead hold A and B at 50% each, introduce C, and
replace B within the experimental half. A's LWS spec, Pod UIDs and subrole labels
must remain unchanged while C starts, waits for readiness, and replaces B.
The final hold requires A and C at 2 Prefill / 4 Decode each, with no B groups.

During a canary, the exact requested split takes precedence over ordinary
Deferred target-retention: both policies converge to the same requested counts.
The tests check observed whole-group availability and physical requested surge,
including a no-worsening Ready floor while demand grows. They do not treat
terminating Pods as new requested capacity or claim atomic API snapshots.

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
  bounds from the Go observer built against the checked-out commit.
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

Capture does not claim continuous LIST/watch or controller log coverage. Nothing
is uploaded automatically. If CI publishes artifacts, explicitly select the
results and recordings needed, and review their contents before sharing: raw
snapshots, events and command diagnostics can contain private data.

**Do not upload the whole `.state/` directory.** It also contains the private
kubeconfig, the ownership token in both `owner` and `runtime.json`, and temporary
source archives, build outputs and other runtime work files. Never publish the
kubeconfig, either copy of the ownership token, or an unreviewed run-directory
archive.

## Read-only planner observer

`observer/testdata/` contains ordinary Go source files. Go excludes `testdata`
from normal package discovery, keeping these diagnostic helpers out of production
builds. The launcher uses a small Go `-overlay` mapping to build the standalone
observer against the HEAD source archive. It gives the diagnostics access to
private planner helpers during that build only, without copying files into the
archive, changing the controller, or adding production source under
`pkg/controllers`.

The observer uses a Reader-only Kubernetes client. Its bounds are independent
observations, not controller write authorization. It uses the planner APIs from
the same checkout, so incompatible API changes fail at build time.

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
