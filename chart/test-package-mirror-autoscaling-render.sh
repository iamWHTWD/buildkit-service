#!/usr/bin/env bash
set -euo pipefail

chart_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

common_args=(--set buildctlDaemon.auth.token=ci-lint-placeholder)

fail() {
  echo "package-mirror autoscaling render test: $*" >&2
  exit 1
}

assert_contains() {
  local needle="$1"
  local file="$2"
  grep -Fq -- "$needle" "$file" || fail "expected '$needle' in $file"
}

assert_absent() {
  local needle="$1"
  local file="$2"
  if grep -Fq -- "$needle" "$file"; then
    fail "did not expect '$needle' in $file"
  fi
}

assert_count() {
  local expected="$1"
  local needle="$2"
  local file="$3"
  local actual
  actual="$(grep -Fc -- "$needle" "$file" || true)"
  [[ "$actual" == "$expected" ]] || fail "expected $expected occurrences of '$needle' in $file, got $actual"
}

helm template package-mirror-test "$chart_dir" "${common_args[@]}" > "$tmp_dir/default.yaml"
helm template package-mirror-test "$chart_dir" "${common_args[@]}" \
  --show-only templates/package-mirror-deployment.yaml > "$tmp_dir/default-deployment.yaml"
helm template package-mirror-test "$chart_dir" "${common_args[@]}" \
  --show-only templates/package-mirror-service.yaml > "$tmp_dir/services.yaml"
helm template package-mirror-test "$chart_dir" "${common_args[@]}" \
  --show-only templates/package-mirror-git-deployment.yaml > "$tmp_dir/git.yaml"
helm template package-mirror-test "$chart_dir" "${common_args[@]}" \
  --show-only templates/package-mirror-registry-deployment.yaml > "$tmp_dir/registry.yaml"

assert_absent "kind: HorizontalPodAutoscaler" "$tmp_dir/default.yaml"
assert_contains "  replicas: 20" "$tmp_dir/default-deployment.yaml"
assert_contains 'cluster-autoscaler.kubernetes.io/safe-to-evict: "true"' "$tmp_dir/default-deployment.yaml"
assert_count 3 "          startupProbe:" "$tmp_dir/default-deployment.yaml"
assert_count 5 "            requests:" "$tmp_dir/default-deployment.yaml"
assert_count 4 "      timeoutSeconds: 900" "$tmp_dir/services.yaml"
assert_absent "cluster-autoscaler.kubernetes.io/safe-to-evict" "$tmp_dir/git.yaml"
assert_absent "cluster-autoscaler.kubernetes.io/safe-to-evict" "$tmp_dir/registry.yaml"

helm template package-mirror-test "$chart_dir" "${common_args[@]}" \
  --set packageMirror.autoscaling.enabled=true \
  --show-only templates/package-mirror-deployment.yaml > "$tmp_dir/autoscaled-deployment.yaml"
helm template package-mirror-test "$chart_dir" "${common_args[@]}" \
  --set packageMirror.autoscaling.enabled=true \
  --show-only templates/package-mirror-hpa.yaml > "$tmp_dir/hpa.yaml"

assert_absent "  replicas:" "$tmp_dir/autoscaled-deployment.yaml"
assert_contains "kind: HorizontalPodAutoscaler" "$tmp_dir/hpa.yaml"
assert_contains "    kind: Deployment" "$tmp_dir/hpa.yaml"
assert_contains "    name: package-mirror" "$tmp_dir/hpa.yaml"
assert_contains "  minReplicas: 20" "$tmp_dir/hpa.yaml"
assert_contains "  maxReplicas: 40" "$tmp_dir/hpa.yaml"
assert_count 6 "    - type: ContainerResource" "$tmp_dir/hpa.yaml"
assert_count 3 "          averageUtilization: 65" "$tmp_dir/hpa.yaml"
assert_contains "          averageValue: 1536Mi" "$tmp_dir/hpa.yaml"
assert_contains "          averageValue: 6Gi" "$tmp_dir/hpa.yaml"
assert_contains "          averageValue: 3Gi" "$tmp_dir/hpa.yaml"
assert_contains "      stabilizationWindowSeconds: 1800" "$tmp_dir/hpa.yaml"

helm template package-mirror-test "$chart_dir" "${common_args[@]}" \
  --set packageMirror.autoscaling.enabled=true \
  --set-string packageMirror.pip.resources.requests.memory=3Gi \
  --set-string packageMirror.pip.resources.limits.memory=3Gi \
  --show-only templates/package-mirror-deployment.yaml > "$tmp_dir/custom-resources.yaml"
helm template package-mirror-test "$chart_dir" "${common_args[@]}" \
  --set packageMirror.autoscaling.enabled=true \
  --set-string packageMirror.autoscaling.metrics.pip.memory.averageValue=2304Mi \
  --show-only templates/package-mirror-hpa.yaml > "$tmp_dir/custom-hpa.yaml"
assert_count 2 "              memory: 3Gi" "$tmp_dir/custom-resources.yaml"
assert_contains "          averageValue: 2304Mi" "$tmp_dir/custom-hpa.yaml"

helm template package-mirror-test "$chart_dir" "${common_args[@]}" \
  --set-string packageMirror.deployment.nodeSelector.node-pool=package-mirror \
  --show-only templates/package-mirror-deployment.yaml > "$tmp_dir/main-node-selector.yaml"
helm template package-mirror-test "$chart_dir" "${common_args[@]}" \
  --set-string packageMirror.deployment.nodeSelector.node-pool=package-mirror \
  --show-only templates/package-mirror-registry-deployment.yaml > "$tmp_dir/registry-node-selector.yaml"
assert_contains "        node-pool: package-mirror" "$tmp_dir/main-node-selector.yaml"
assert_absent "node-pool: package-mirror" "$tmp_dir/registry-node-selector.yaml"

helm template package-mirror-quickstart "$chart_dir" "${common_args[@]}" \
  --values "$chart_dir/values-quickstart.yaml" \
  --show-only templates/package-mirror-deployment.yaml > "$tmp_dir/quickstart-deployment.yaml"
assert_contains "  replicas: 1" "$tmp_dir/quickstart-deployment.yaml"
assert_count 5 "            requests:" "$tmp_dir/quickstart-deployment.yaml"

if helm template package-mirror-test "$chart_dir" "${common_args[@]}" \
  --set packageMirror.autoscaling.enabled=true \
  --set packageMirror.autoscaling.minReplicas=41 \
  --set packageMirror.autoscaling.maxReplicas=40 >/dev/null 2>&1; then
  fail "expected minReplicas greater than maxReplicas to fail rendering"
fi

echo "package-mirror autoscaling render test: PASS"
