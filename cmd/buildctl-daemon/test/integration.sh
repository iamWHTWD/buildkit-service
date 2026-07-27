#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH='' cd -- "${SCRIPT_DIR}/../../.." && pwd)
# Keep bind-mounted fixtures under the repository by default. On macOS Colima
# shares /Users with its VM, while the host's /var/folders TMPDIR is not shared.
INTEGRATION_TMP_ROOT=${BUILDCTL_DAEMON_INTEGRATION_TMPDIR:-${REPO_ROOT}}
TMP_DIR=$(mktemp -d "${INTEGRATION_TMP_ROOT}/.buildctl-daemon-integration.XXXXXX")
RUN_ID="buildctl-it-${TMP_DIR##*.}"
RUN_ID=$(printf '%s' "${RUN_ID}" | tr '[:upper:]' '[:lower:]')
NETWORK="${RUN_ID}-net"
REGISTRY_A="${RUN_ID}-registry-a"
REGISTRY_B="${RUN_ID}-registry-b"
BUILDKIT="${RUN_ID}-buildkit"
BUILDKIT_IMAGE="${BUILDCTL_DAEMON_INTEGRATION_BUILDKIT_IMAGE:-moby/buildkit:v0.22.0}"
DAEMON_PORT="${BUILDCTL_DAEMON_INTEGRATION_PORT:-18080}"
DAEMON_PID=""

cleanup() {
	integration_status=$?
	if [[ ${integration_status} -ne 0 ]]; then
		echo "integration test failed; diagnostic logs follow" >&2
		docker inspect "${BUILDKIT}" >&2 2>/dev/null || true
		docker logs "${BUILDKIT}" >&2 2>/dev/null || true
		if [[ -f "${TMP_DIR}/daemon.log" ]]; then
			echo "buildctl-daemon log:" >&2
			sed -n '1,240p' "${TMP_DIR}/daemon.log" >&2
		fi
		if [[ -n "${BOTH_RESPONSE:-}" ]]; then
			echo "both response: ${BOTH_RESPONSE}" >&2
			both_id=$(jq -r '.id // empty' <<<"${BOTH_RESPONSE}")
			if [[ -n "${both_id}" ]]; then
				echo "both task log:" >&2
				curl -sS "http://127.0.0.1:${DAEMON_PORT}/v1/builds/${both_id}/logs?tail_bytes=50000" >&2 || true
			fi
		fi
	fi
	if [[ -n "${DAEMON_PID}" ]]; then
		kill "${DAEMON_PID}" >/dev/null 2>&1 || true
		wait "${DAEMON_PID}" >/dev/null 2>&1 || true
	fi
	docker rm -f "${BUILDKIT}" "${REGISTRY_A}" "${REGISTRY_B}" >/dev/null 2>&1 || true
	docker network rm "${NETWORK}" >/dev/null 2>&1 || true
	rm -rf -- "${TMP_DIR}"
	exit "${integration_status}"
}
trap cleanup EXIT

for command in docker go curl jq zip; do
	command -v "${command}" >/dev/null 2>&1 || {
		echo "missing required command: ${command}" >&2
		exit 1
	}
done

docker info >/dev/null
docker network create "${NETWORK}" >/dev/null
docker run -d --name "${REGISTRY_A}" --network "${NETWORK}" -p 127.0.0.1::5000 registry:2 >/dev/null
docker run -d --name "${REGISTRY_B}" --network "${NETWORK}" -p 127.0.0.1::5000 registry:2 >/dev/null

REGISTRY_A_PORT=$(docker port "${REGISTRY_A}" 5000/tcp | sed -n 's/.*://p')
REGISTRY_B_PORT=$(docker port "${REGISTRY_B}" 5000/tcp | sed -n 's/.*://p')

for registry_port in "${REGISTRY_A_PORT}" "${REGISTRY_B_PORT}"; do
	for _ in $(seq 1 30); do
		if curl -fsS "http://127.0.0.1:${registry_port}/v2/" >/dev/null 2>&1; then
			break
		fi
		sleep 1
	done
	curl -fsS "http://127.0.0.1:${registry_port}/v2/" >/dev/null
done

cat >"${TMP_DIR}/buildkitd.toml" <<EOF
[registry."${REGISTRY_A}:5000"]
  http = true
  insecure = true

[registry."${REGISTRY_B}:5000"]
  http = true
  insecure = true
EOF

docker run -d --privileged --name "${BUILDKIT}" --network "${NETWORK}" \
	-p 127.0.0.1::1234 \
	-v "${TMP_DIR}/buildkitd.toml:/etc/buildkit/buildkitd.toml:ro" \
	"${BUILDKIT_IMAGE}" \
	--config /etc/buildkit/buildkitd.toml \
	--addr tcp://0.0.0.0:1234 >/dev/null

BUILDKIT_PORT=$(docker port "${BUILDKIT}" 1234/tcp | sed -n 's/.*://p')

for _ in $(seq 1 60); do
	if docker exec "${BUILDKIT}" buildctl --addr tcp://127.0.0.1:1234 debug workers >/dev/null 2>&1; then
		break
	fi
	sleep 1
done
docker exec "${BUILDKIT}" buildctl --addr tcp://127.0.0.1:1234 debug workers >/dev/null

mkdir -p "${TMP_DIR}/context"
cat >"${TMP_DIR}/context/Dockerfile" <<'EOF'
FROM scratch
COPY payload.txt /payload.txt
LABEL buildctl-daemon-integration=true
EOF
printf 'buildctl-daemon-integration\n' >"${TMP_DIR}/context/payload.txt"
(
	cd "${TMP_DIR}/context"
	zip -q "${TMP_DIR}/source.zip" Dockerfile payload.txt
)

MODES_JSON='{"default_mode":{"concurrency":1,"routingEnabled":false},"production_mode":{"concurrency":1,"routingEnabled":true}}'
TARGETS_JSON=$(jq -cn --arg a "${REGISTRY_A}:5000" --arg b "${REGISTRY_B}:5000" '[$a,$b]')

cd "${REPO_ROOT}"
env GOCACHE="${TMP_DIR}/go-cache" go build -o "${TMP_DIR}/buildctl-daemon" ./cmd/buildctl-daemon
"${TMP_DIR}/buildctl-daemon" \
	--listen "127.0.0.1:${DAEMON_PORT}" \
	--buildkitd-addr "tcp://127.0.0.1:${BUILDKIT_PORT}" \
	--work-dir "${TMP_DIR}/daemon" \
	--default-mode default_mode \
	--modes-json "${MODES_JSON}" \
	--routing-targets-json "${TARGETS_JSON}" \
	>"${TMP_DIR}/daemon.log" 2>&1 &
DAEMON_PID=$!

for _ in $(seq 1 60); do
	if curl -fsS "http://127.0.0.1:${DAEMON_PORT}/healthz" >/dev/null 2>&1; then
		break
	fi
	sleep 1
done
curl -fsS "http://127.0.0.1:${DAEMON_PORT}/healthz" >/dev/null

LEGACY_IMAGE="${REGISTRY_A}:5000/buildctl-integration/legacy:${RUN_ID}"
LEGACY_RESPONSE=$(curl -fsS -X POST \
	-F "file=@${TMP_DIR}/source.zip" \
	-F "image=${LEGACY_IMAGE}" \
	-F image_type=oci \
	-F sync=true \
	-F retry-interval=0 \
	"http://127.0.0.1:${DAEMON_PORT}/v1/builds")

jq -e --arg image "${LEGACY_IMAGE}" \
	'.status == "succeeded" and .mode == "default_mode" and .image == $image and .routed_target == $image' \
	<<<"${LEGACY_RESPONSE}" >/dev/null

PRODUCTION_RESPONSE=$(curl -fsS -X POST \
	-F "file=@${TMP_DIR}/source.zip" \
	-F "image=logical.invalid/buildctl-integration/production:${RUN_ID}" \
	-F image_type=oci \
	-F mode=production_mode \
	-F sync=true \
	-F retry-interval=0 \
	"http://127.0.0.1:${DAEMON_PORT}/v1/builds")

jq -e '.status == "succeeded" and .mode == "production_mode" and .routed_target != .image' \
	<<<"${PRODUCTION_RESPONSE}" >/dev/null
ROUTED_TARGET=$(jq -r '.routed_target' <<<"${PRODUCTION_RESPONSE}")
ROUTED_HOST=${ROUTED_TARGET%%/*}
ROUTED_REPOSITORY_TAG=${ROUTED_TARGET#*/}
ROUTED_REPOSITORY=${ROUTED_REPOSITORY_TAG%:*}
ROUTED_TAG=${ROUTED_REPOSITORY_TAG##*:}

case "${ROUTED_HOST}" in
"${REGISTRY_A}:5000") ROUTED_PORT=${REGISTRY_A_PORT} ;;
"${REGISTRY_B}:5000") ROUTED_PORT=${REGISTRY_B_PORT} ;;
*)
	echo "unexpected routed host: ${ROUTED_HOST}" >&2
	exit 1
	;;
esac

curl -fsS -o /dev/null \
	-H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
	"http://127.0.0.1:${ROUTED_PORT}/v2/${ROUTED_REPOSITORY}/manifests/${ROUTED_TAG}"

BOTH_RESPONSE=$(curl -fsS -X POST \
	-F "file=@${TMP_DIR}/source.zip" \
	-F "image=logical.invalid/buildctl-integration/both:${RUN_ID}" \
	-F image_type=both \
	-F mode=production_mode \
	-F sync=true \
	-F retry=0 \
	-F retry-interval=0 \
	"http://127.0.0.1:${DAEMON_PORT}/v1/builds")

jq -e '.mode == "production_mode" and .image_type == "both" and .routed_target != .image' \
	<<<"${BOTH_RESPONSE}" >/dev/null
BOTH_STATUS=$(jq -r '.status' <<<"${BOTH_RESPONSE}")
BOTH_ERROR=$(jq -r '.error // ""' <<<"${BOTH_RESPONSE}")
BOTH_ID=$(jq -r '.id' <<<"${BOTH_RESPONSE}")
BOTH_ROUTED_TARGET=$(jq -r '.routed_target' <<<"${BOTH_RESPONSE}")
BOTH_ROUTED_HOST=${BOTH_ROUTED_TARGET%%/*}
BOTH_REPOSITORY_TAG=${BOTH_ROUTED_TARGET#*/}
BOTH_REPOSITORY=${BOTH_REPOSITORY_TAG%:*}
BOTH_TAG=${BOTH_REPOSITORY_TAG##*:}

case "${BOTH_ROUTED_HOST}" in
"${REGISTRY_A}:5000") BOTH_ROUTED_PORT=${REGISTRY_A_PORT} ;;
"${REGISTRY_B}:5000") BOTH_ROUTED_PORT=${REGISTRY_B_PORT} ;;
*)
	echo "unexpected both routed host: ${BOTH_ROUTED_HOST}" >&2
	exit 1
	;;
esac

curl -fsS -o /dev/null \
	-H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
	"http://127.0.0.1:${BOTH_ROUTED_PORT}/v2/${BOTH_REPOSITORY}/manifests/${BOTH_TAG}"

BOTH_LOG=$(curl -fsS "http://127.0.0.1:${DAEMON_PORT}/v1/builds/${BOTH_ID}/logs?tail_bytes=20000")
OCI_LOG_LINE=$(grep -nF "build oci image ${BOTH_ROUTED_TARGET} on" <<<"${BOTH_LOG}" | sed -n '1s/:.*//p')
FROM_LOG_LINE=$(grep -nF "build nydus from OCI image ${BOTH_ROUTED_TARGET}@sha256:" <<<"${BOTH_LOG}" | sed -n '1s/:.*//p')
NYDUS_LOG_LINE=$(grep -nF "build nydus image ${BOTH_ROUTED_TARGET}_nydus_v3 on" <<<"${BOTH_LOG}" | sed -n '1s/:.*//p')
if [[ -z "${OCI_LOG_LINE}" || -z "${FROM_LOG_LINE}" || -z "${NYDUS_LOG_LINE}" ||
	${OCI_LOG_LINE} -ge ${FROM_LOG_LINE} || ${FROM_LOG_LINE} -ge ${NYDUS_LOG_LINE} ]]; then
	echo "both build did not run OCI -> FROM pinned OCI -> Nydus in order" >&2
	printf '%s\n' "${BOTH_LOG}" >&2
	exit 1
fi

if [[ "${BOTH_STATUS}" == "succeeded" ]]; then
	curl -fsS -o /dev/null \
		-H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
		"http://127.0.0.1:${BOTH_ROUTED_PORT}/v2/${BOTH_REPOSITORY}/manifests/${BOTH_TAG}_nydus_v3"
	BOTH_VALIDATION="full OCI and Nydus manifest validation"
elif [[ "${BUILDKIT_IMAGE}" == "moby/buildkit:v0.22.0" && "${BOTH_ERROR}" == *"unsupported compression type nydus"* ]]; then
	BOTH_VALIDATION="OCI and pinned-FROM orchestration validation; vanilla BuildKit has no Nydus exporter"
else
	echo "both build failed unexpectedly with BuildKit image ${BUILDKIT_IMAGE}: ${BOTH_ERROR}" >&2
	exit 1
fi

echo "integration test passed"
echo "legacy response: ${LEGACY_RESPONSE}"
echo "production response: ${PRODUCTION_RESPONSE}"
echo "both response: ${BOTH_RESPONSE}"
echo "both validation: ${BOTH_VALIDATION}"
