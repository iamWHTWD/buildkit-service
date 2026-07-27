#!/usr/bin/env bash
# preheat.sh - trigger Dragonfly image preheating (fire-and-forget, multi-user
# friendly).
#
# Each run creates a uniquely-named Job: the Pod parses the image list line by
# line and calls Scheduler.PreheatImage with the bundled grpcurl. The image list is
# injected base64-encoded via a Job env var. Private-registry credentials are
# read from an existing Kubernetes Secret in the target namespace.
# The script returns immediately after submitting the Job — preheating runs on
# the cluster side, and ttlSecondsAfterFinished reclaims the Job and Pod after
# completion, no local waiting required.
#
# Usage:
#   ./preheat.sh --image-list ./images.txt --registry-secret registry-auth
#
# See --help for options.

set -euo pipefail

# ---- defaults ----
IMAGE_LIST=""
REGISTRY_SECRET=""
INSECURE_SKIP_VERIFY="false"
NAMESPACE="default"
NAME=""
SCHEDULER_ADDR="dragonfly-scheduler.dragonfly-system.svc.cluster.local:8002"
SCHEDULER_TLS="false"
SCHEDULER_CA_SECRET=""
SCHEDULER_SERVER_NAME=""
INTERVAL_SECONDS="10"
RUNNER_IMAGE="ghcr.io/inclusionai/buildkit-service/preheat:latest"
KUBECONFIG_PATH="${KUBECONFIG:-}"
TTL_SECONDS="600"
MAX_IMAGE_LIST_BYTES=65536

die() {
	echo "Error: $*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Usage: preheat.sh --image-list FILE [options]

Required:
  --image-list FILE        image list file, one image reference per line
                           (# and blank lines ignored)

Auth (optional, for private registries):
  --registry-secret NAME   Secret in the target namespace containing the
                           username and password keys

Options:
  --namespace NS           target namespace (default: default)
  --name NAME              Job name (default: preheat-<random suffix>; do not
                           fix the name in multi-user scenarios)
  --scheduler-addr ADDR    Dragonfly Scheduler gRPC address
                           (default: dragonfly-scheduler.dragonfly-system.svc.cluster.local:8002)
  --scheduler-tls          use TLS for the Scheduler connection
  --scheduler-ca-secret NAME
                           Secret containing ca.crt in the target namespace
  --scheduler-server-name NAME
                           TLS server name (defaults to Scheduler host)
  --interval SECONDS       seconds between two preheats (default: 10)
  --ttl SECONDS            seconds before the finished Job is reclaimed (default: 600)
  --image IMAGE            helper image for the Job
                           (default: ghcr.io/inclusionai/buildkit-service/preheat:latest)
  --kubeconfig FILE        kubeconfig path (default: $KUBECONFIG)
  --insecure-skip-verify   skip registry TLS verification (unsafe)
  -h, --help               show this help
EOF
}

# ---- parse args ----
while [[ $# -gt 0 ]]; do
	case "$1" in
	--image-list) IMAGE_LIST="$2"; shift 2 ;;
	--image-list=*) IMAGE_LIST="${1#*=}"; shift ;;
  --registry-secret) REGISTRY_SECRET="$2"; shift 2 ;;
  --registry-secret=*) REGISTRY_SECRET="${1#*=}"; shift ;;
	--namespace) NAMESPACE="$2"; shift 2 ;;
	--namespace=*) NAMESPACE="${1#*=}"; shift ;;
	--name) NAME="$2"; shift 2 ;;
	--name=*) NAME="${1#*=}"; shift ;;
  --scheduler-addr) SCHEDULER_ADDR="$2"; shift 2 ;;
	--scheduler-addr=*) SCHEDULER_ADDR="${1#*=}"; shift ;;
  --scheduler-tls) SCHEDULER_TLS="true"; shift ;;
  --scheduler-ca-secret) SCHEDULER_CA_SECRET="$2"; shift 2 ;;
  --scheduler-ca-secret=*) SCHEDULER_CA_SECRET="${1#*=}"; shift ;;
  --scheduler-server-name) SCHEDULER_SERVER_NAME="$2"; shift 2 ;;
  --scheduler-server-name=*) SCHEDULER_SERVER_NAME="${1#*=}"; shift ;;
	--interval) INTERVAL_SECONDS="$2"; shift 2 ;;
	--interval=*) INTERVAL_SECONDS="${1#*=}"; shift ;;
	--ttl) TTL_SECONDS="$2"; shift 2 ;;
	--ttl=*) TTL_SECONDS="${1#*=}"; shift ;;
	--image) RUNNER_IMAGE="$2"; shift 2 ;;
	--image=*) RUNNER_IMAGE="${1#*=}"; shift ;;
	--kubeconfig) KUBECONFIG_PATH="$2"; shift 2 ;;
	--kubeconfig=*) KUBECONFIG_PATH="${1#*=}"; shift ;;
  --insecure-skip-verify) INSECURE_SKIP_VERIFY="true"; shift ;;
  --username|--username=*|--password|--password=*)
    die "$1 is not supported; store credentials in a Secret and use --registry-secret"
    ;;
	-h|--help) usage; exit 0 ;;
	*) die "unknown flag: $1 (see --help)" ;;
	esac
done

# ---- validate ----
command -v kubectl >/dev/null 2>&1 || die "kubectl not found in PATH"
[[ -n "${IMAGE_LIST}" ]] || die "--image-list is required (see --help)"
[[ -f "${IMAGE_LIST}" ]] || die "image list file not found: ${IMAGE_LIST}"
[[ "${INTERVAL_SECONDS}" =~ ^[0-9]+$ ]] || die "--interval must be a non-negative integer"
[[ "${TTL_SECONDS}" =~ ^[0-9]+$ ]] || die "--ttl must be a non-negative integer"
[[ "${NAMESPACE}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || die "--namespace must be a valid Kubernetes namespace"
[[ "${SCHEDULER_ADDR}" =~ ^[A-Za-z0-9.-]+:[0-9]+$ ]] || die "--scheduler-addr must be HOST:PORT"
[[ "${RUNNER_IMAGE}" =~ ^[A-Za-z0-9._/@:-]+$ ]] || die "--image contains unsupported characters"
if [[ -n "${REGISTRY_SECRET}" ]]; then
  [[ "${REGISTRY_SECRET}" =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]] || die "--registry-secret must be a valid Secret name"
  [[ "${SCHEDULER_TLS}" == "true" ]] || die "--registry-secret requires --scheduler-tls so credentials are not sent over plaintext gRPC"
fi
if [[ -n "${SCHEDULER_CA_SECRET}" ]]; then
  [[ "${SCHEDULER_TLS}" == "true" ]] || die "--scheduler-ca-secret requires --scheduler-tls"
  [[ "${SCHEDULER_CA_SECRET}" =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]] || die "--scheduler-ca-secret must be a valid Secret name"
fi
if [[ -n "${SCHEDULER_SERVER_NAME}" ]]; then
  [[ "${SCHEDULER_SERVER_NAME}" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] || die "--scheduler-server-name must be a DNS name"
fi

IMAGE_LIST_BYTES=$(wc -c < "${IMAGE_LIST}")
[[ "${IMAGE_LIST_BYTES}" -le "${MAX_IMAGE_LIST_BYTES}" ]] || die "image list exceeds ${MAX_IMAGE_LIST_BYTES} bytes"

if [[ -z "${NAME}" ]]; then
	NAME="preheat-$(LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 8 || echo "$$")"
fi
JOB_NAME="${NAME}"
[[ "${JOB_NAME}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || die "--name must be a valid Kubernetes Job name"

# The image list is inlined base64-encoded into the Job (no ConfigMap),
# avoiding newline/indentation issues.
IMAGES_B64="$(base64 < "${IMAGE_LIST}" | tr -d '\n')"

KUBECTL=(kubectl)
[[ -n "${KUBECONFIG_PATH}" ]] && KUBECTL+=(--kubeconfig "${KUBECONFIG_PATH}")
KUBECTL+=(--namespace "${NAMESPACE}")

echo ">> namespace=${NAMESPACE} job=${JOB_NAME} scheduler=${SCHEDULER_ADDR} interval=${INTERVAL_SECONDS}s ttl=${TTL_SECONDS}s"

render_registry_auth_env() {
	if [[ -n "${REGISTRY_SECRET}" ]]; then
		cat <<EOF
            - name: REGISTRY_USERNAME
              valueFrom:
                secretKeyRef:
                  name: ${REGISTRY_SECRET}
                  key: username
            - name: REGISTRY_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: ${REGISTRY_SECRET}
                  key: password
EOF
	else
		cat <<'EOF'
            - name: REGISTRY_USERNAME
              value: ""
            - name: REGISTRY_PASSWORD
              value: ""
EOF
	fi
}

render_scheduler_tls_mount() {
	if [[ -n "${SCHEDULER_CA_SECRET}" ]]; then
		cat <<'EOF'
          volumeMounts:
            - name: scheduler-ca
              mountPath: /etc/dragonfly-tls
              readOnly: true
EOF
	fi
}

render_scheduler_tls_volume() {
	if [[ -n "${SCHEDULER_CA_SECRET}" ]]; then
		cat <<EOF
      volumes:
        - name: scheduler-ca
          secret:
            secretName: ${SCHEDULER_CA_SECRET}
            items:
              - key: ca.crt
                path: ca.crt
EOF
	fi
}

# ---- Create the uniquely-named Job: install grpcurl -> decode the image list
# ---- -> preheat one by one; TTL reclaims the Job after completion ----
"${KUBECTL[@]}" apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${JOB_NAME}
  namespace: ${NAMESPACE}
  labels:
    app: preheat
spec:
  ttlSecondsAfterFinished: ${TTL_SECONDS}
  backoffLimit: 0
  template:
    metadata:
      labels:
        app: preheat
    spec:
      automountServiceAccountToken: false
      securityContext:
        seccompProfile:
          type: RuntimeDefault
      restartPolicy: Never
      containers:
        - name: grpcurl
          image: ${RUNNER_IMAGE}
          env:
            - name: SCHEDULER_ADDR
              value: "${SCHEDULER_ADDR}"
            - name: SCHEDULER_TLS
              value: "${SCHEDULER_TLS}"
            - name: SCHEDULER_SERVER_NAME
              value: "${SCHEDULER_SERVER_NAME}"
            - name: INTERVAL_SECONDS
              value: "${INTERVAL_SECONDS}"
            - name: INSECURE_SKIP_VERIFY
              value: "${INSECURE_SKIP_VERIFY}"
$(render_registry_auth_env)
            - name: IMAGES_B64
              value: "${IMAGES_B64}"
          command: ["/bin/bash", "-c"]
$(render_scheduler_tls_mount)
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
          args:
            - |
              set -euo pipefail
              grpcurl --version

              grpcurl_args=()
              if [[ "\${SCHEDULER_TLS}" == "true" ]]; then
                [[ ! -f /etc/dragonfly-tls/ca.crt ]] || grpcurl_args+=( -cacert /etc/dragonfly-tls/ca.crt )
                [[ -z "\${SCHEDULER_SERVER_NAME}" ]] || grpcurl_args+=( -servername "\${SCHEDULER_SERVER_NAME}" )
              else
                grpcurl_args+=( -plaintext )
              fi

              echo "\${IMAGES_B64}" | base64 -d > /tmp/images.txt

              # HOST/NS/REPO:TAG  ->  https://HOST/v2/NS/REPO/manifests/TAG
              build_url() {
                local image="\$1" host rest repo ref
                host="\${image%%/*}"
                rest="\${image#*/}"
                if [[ "\${host}" == "\${image}" ]]; then
                  echo "ERR: not a fully qualified image: \${image}" >&2
                  return 1
                fi
                if [[ "\${rest}" == *"@"* ]]; then
                  ref="\${rest##*@}"; repo="\${rest%@*}"
                elif [[ "\${rest##*/}" == *":"* ]]; then
                  ref="\${rest##*:}"; repo="\${rest%:*}"
                else
                  ref="latest"; repo="\${rest}"
                fi
                echo "https://\${host}/v2/\${repo}/manifests/\${ref}"
              }

              total=0; ok=0; fail=0
              while IFS= read -r line || [[ -n "\${line}" ]]; do
                image="\$(echo "\${line}" | sed 's/#.*//' | xargs || true)"
                [[ -z "\${image}" ]] && continue
                total=\$((total+1))
                if ! url="\$(build_url "\${image}")"; then
                  echo "[SKIP] \${image} (parse failed)"; fail=\$((fail+1)); continue
                fi
                echo "[PREHEAT] \${image} -> \${url}"
                req=\$(jq -nc \
                  --arg url "\${url}" \
                  --argjson insecureSkipVerify "\${INSECURE_SKIP_VERIFY}" \
                  '{url: \$url, username: env.REGISTRY_USERNAME, password: env.REGISTRY_PASSWORD, scope: "all_seed_peers", insecureSkipVerify: \$insecureSkipVerify}')
                 if printf '%s' "\${req}" | grpcurl "\${grpcurl_args[@]}" -d @ \
                     "\${SCHEDULER_ADDR}" scheduler.v2.Scheduler.PreheatImage; then
                  echo "[OK] \${image}"; ok=\$((ok+1))
                else
                  echo "[FAIL] \${image}"; fail=\$((fail+1))
                fi
                sleep "\${INTERVAL_SECONDS}"
              done < /tmp/images.txt

              echo "Done. total=\${total} ok=\${ok} fail=\${fail}"
              [[ "\${fail}" -eq 0 ]]
          resources:
            requests:
              cpu: "100m"
              memory: "128Mi"
            limits:
              cpu: "500m"
              memory: "256Mi"
$(render_scheduler_tls_volume)
EOF

cat <<EOF
>> Job ${JOB_NAME} submitted; it runs in the cluster and self-cleans ${TTL_SECONDS}s after finishing.
>> Track progress:
     kubectl -n ${NAMESPACE} logs -f job/${JOB_NAME}
>> Check status:
     kubectl -n ${NAMESPACE} get job ${JOB_NAME}
EOF
