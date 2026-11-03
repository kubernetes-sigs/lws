#!/usr/bin/env bash
# Produce a build-only overlay. Never copy diagnostic hooks into controller source.
set -euo pipefail

fail() { printf '%s\n' "$*" >&2; exit 1; }
[[ "$#" == 2 ]] || fail 'Usage: prepare.sh SOURCE_ROOT OVERLAY_JSON'
SOURCE_ROOT="$(cd "$1" && pwd)"
ASSETS="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUTPUT="$2"
CONTROLLER="$SOURCE_ROOT/pkg/controllers/disaggregatedset"
[[ -f "$SOURCE_ROOT/go.mod" && -f "$CONTROLLER/executor.go" && -f "$CONTROLLER/planner_state.go" ]] || fail 'Unsupported observer source layout.'
[[ ! -e "$OUTPUT" && ! -L "$OUTPUT" ]] || fail 'Overlay output already exists; choose a new file.'

# Build with -tags=harness. Go ignores *_test.go during ordinary builds.
jq -n --arg source "$SOURCE_ROOT" --arg assets "$ASSETS" '
  $ARGS.positional |
  map({key:($source + "/pkg/controllers/disaggregatedset/" + (split("/") | last)), value:.}) |
  from_entries |
  . + {($source + "/test/disaggregatedset_harness/observer/main.go"):($assets + "/testdata/main.go")} |
  {Replace:.}
' --args "$ASSETS"/testdata/controller/*.go > "$OUTPUT"
