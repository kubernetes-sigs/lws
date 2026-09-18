#!/bin/bash
set -e

# CRDs that require a conversion webhook clientConfig.
# After install/replace, spec.conversion is patched with the correct service
# name and namespace so Kubernetes routes conversion requests to the webhook.
CONVERSION_CRDS=(
  "leaderworkersets.leaderworkerset.x-k8s.io"
)

# Namespace and service name for the conversion webhook.
# Override via environment variables when invoking this script.
WEBHOOK_NAMESPACE="${WEBHOOK_NAMESPACE:-lws-system}"
WEBHOOK_SERVICE="${WEBHOOK_SERVICE:-lws-webhook-service}"

# Collect CRD names from the bundled YAML files.
CRD_NAMES=()
while IFS= read -r -d '' crdfile; do
  crd_name=$(grep -m1 '^  name:' "$crdfile" | awk '{print $2}')
  [[ -n "$crd_name" ]] && CRD_NAMES+=("$crd_name")
done < <(find /lws/crds -maxdepth 1 -name '*.yaml' -print0)

###############################################################################
# Phase 1 — Apply CRDs (create or replace)
###############################################################################
echo "=== Phase 1: Applying CRDs ==="

for crd_name in "${CRD_NAMES[@]}"; do
  # Find the matching file.
  crdfile=$(find /lws/crds -maxdepth 1 -name '*.yaml' | while read -r f; do
    grep -q "name: $crd_name" "$f" && echo "$f"
  done | head -1)
  crdshort=${crdfile##*/}

  if kubectl get crd "$crd_name" >/dev/null 2>&1; then
    echo "$crdshort found, replacing CRD..."
    kubectl replace -f "$crdfile"
  else
    echo "$crdshort not found, creating CRD..."
    kubectl create -f "$crdfile"
  fi
done

###############################################################################
# Phase 2 — Patch conversion webhooks
###############################################################################

# Patch spec.conversion on CRDs that use the conversion webhook.
# The base CRD files (from controller-gen) do not include spec.conversion;
# it must be applied separately, mirroring what Kustomize does via
# config/crd/patches/webhook_in_leaderworkersets.yaml.
echo ""
echo "=== Phase 2: Patching conversion webhooks ==="

CONVERSION_PATCH=$(cat <<EOF
{"spec":{"conversion":{"strategy":"Webhook","webhook":{"conversionReviewVersions":["v1"],"clientConfig":{"service":{"namespace":"${WEBHOOK_NAMESPACE}","name":"${WEBHOOK_SERVICE}","path":"/convert"}}}}}}
EOF
)

for crd in "${CONVERSION_CRDS[@]}"; do
  echo "Patching conversion webhook on CRD: $crd"
  kubectl patch crd "$crd" --type=merge -p "${CONVERSION_PATCH}"
done

###############################################################################
# Phase 3 — Protect CRDs from Helm deletion
#
# When CRDs were previously tracked under templates/ (e.g. v0.7.0) but are
# now managed by this hook Job, Helm's manifest-diff logic would delete them
# on upgrade because they no longer appear in the rendered templates.
# Patching helm.sh/resource-policy: keep tells Helm to skip deletion for
# these CRDs, preserving existing custom resources.
#
# IMPORTANT: This must run AFTER Phase 1 (replace) and Phase 2 (webhook patch)
# because kubectl replace overwrites all annotations, which would strip
# the resource-policy annotation if applied earlier.
# Ref: kubernetes-sigs/secrets-store-csi-driver#656
###############################################################################
echo ""
echo "=== Phase 3: Protecting CRDs from Helm deletion ==="

for crd_name in "${CRD_NAMES[@]}"; do
  if kubectl get crd "$crd_name" >/dev/null 2>&1; then
    echo "Annotating $crd_name with helm.sh/resource-policy: keep"
    kubectl annotate crd "$crd_name" "helm.sh/resource-policy=keep" --overwrite 2>&1
  fi
done

echo ""
echo "CRD upgrade complete."
