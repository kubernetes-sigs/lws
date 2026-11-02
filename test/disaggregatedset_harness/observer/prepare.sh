#!/usr/bin/env bash
# Produce a build-only overlay. Never copy diagnostic hooks into controller source.
set -euo pipefail

fail() { printf '%s\n' "$*" >&2; exit 1; }
[[ "$#" == 2 || ( "$#" == 3 && "$3" == --tests ) ]] || fail 'Usage: prepare.sh SOURCE_ROOT OVERLAY_JSON [--tests]'
SOURCE_ROOT="$(cd "$1" && pwd)"
ASSETS="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUTPUT="$2"
CONTROLLER="$SOURCE_ROOT/pkg/controllers/disaggregatedset"
[[ -f "$SOURCE_ROOT/go.mod" && -f "$CONTROLLER/executor.go" && -f "$CONTROLLER/planner_state.go" ]] || fail 'Unsupported observer source layout.'
[[ ! -e "$OUTPUT" && ! -L "$OUTPUT" ]] || fail 'Overlay output already exists; choose a new file.'

# Both known planner declarations must agree. A failed build never retries with
# legacy semantics: sources without the independent scale-up policy opt in here.
INPUT_SCALE_UP=0
STATE_SCALE_UP=0
grep -Eq '^[[:space:]]*scaleUpDuringRollout[[:space:]]+bool([[:space:]]|$)' "$CONTROLLER/executor.go" && INPUT_SCALE_UP=1
grep -Eq '^[[:space:]]*ScaleUpDuringRollout[[:space:]]+bool([[:space:]]|$)' "$CONTROLLER/planner_state.go" && STATE_SCALE_UP=1
[[ "$INPUT_SCALE_UP" == "$STATE_SCALE_UP" ]] || fail 'Unsupported observer scale-up policy declarations.'
TAGS=harness
if (( ! INPUT_SCALE_UP )); then TAGS=harness,harness_legacy_scale_up; fi

jq -n --arg source "$SOURCE_ROOT" --arg assets "$ASSETS" --argjson tests "$([[ "${3:-}" == --tests ]] && printf true || printf false)" '
  ["harness_diagnostics.go", "harness_policy_current.go", "harness_policy_legacy.go"] +
  (if $tests then ["harness_diagnostics_test.go", "harness_policy_current_test.go"] else [] end) |
  map({key:($source + "/pkg/controllers/disaggregatedset/" + .), value:($assets + "/overlay/" + . + ".inc")}) |
  from_entries |
  . + {($source + "/test/disaggregatedset_harness/observer/main.go"):($assets + "/main.go.inc")} |
  {Replace:.}
' > "$OUTPUT"
printf '%s\n' "$TAGS"
