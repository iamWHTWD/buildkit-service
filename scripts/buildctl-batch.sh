#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
STATE_FILE="${SCRIPT_DIR}/.buildctl-batch.state"
BUILDKIT_SERVICE_ADDR="${BUILDCTL_BATCH_BUILDKIT_ADDR:-tcp://buildkit-service.buildkit-service.svc:9094}"
BUILDKIT_SERVICE_NAMESPACE="${BUILDCTL_BATCH_SERVICE_NAMESPACE:-buildkit-service}"
BUILDKIT_SERVICE_DEPLOYMENT="${BUILDCTL_BATCH_SERVICE_DEPLOYMENT:-buildkit-service}"
DEFAULT_NAMESPACE="${BUILDCTL_BATCH_NAMESPACE:-default}"
DEFAULT_WAIT_TIMEOUT="${BUILDCTL_BATCH_WAIT_TIMEOUT:-180s}"
DAEMON_READY_TIMEOUT_SECONDS="${BUILDCTL_BATCH_DAEMON_READY_TIMEOUT:-60}"
DAEMON_SOCKET="/tmp/buildctl-batch.sock"
RUNNER_IMAGE="${BUILDCTL_BATCH_IMAGE:-ghcr.io/inclusionai/buildkit-service/buildctl-daemon:latest}"
IMAGE_PULL_SECRET="${BUILDCTL_BATCH_IMAGE_PULL_SECRET:-}"
WAIT_BUILD_INTERVAL_SECONDS="${BUILDCTL_BATCH_STATUS_INTERVAL:-5}"
DAEMON_PPROF_SERVER="${BUILDCTL_BATCH_DAEMON_PPROF_SERVER:-}"

KUBECONFIG_PATH=""
NAMESPACE="${DEFAULT_NAMESPACE}"
POD_NAME=""
WAIT_TIMEOUT="${DEFAULT_WAIT_TIMEOUT}"

usage() {
	cat <<-'EOF'
	Usage:
	  buildctl-batch [global options] <subcommand> [args...]

	Subcommands:
	  prepare [--image IMAGE] [--image-pull-secret NAME] Create Pod running buildctl-batch daemon, wait until ready
	  list [kubectl flags...]   Pass through to kubectl get pods
	  scale [--target N]        Scale deployment/buildkit-service or show buildkit-service pods
	  build [-- flags...]       Upload .zip from stdin, request async build, then exit
	  build --ttl X [-- flags]  One-shot mode: create a self-cleaning Job (no prepare), build, then exit
	  status                    Print current build status JSON from the selected Pod
	  wait                      Wait until the current build leaves running state
	  logs [kubectl flags...]   Pass through to kubectl logs for the selected Pod
	  exec [kubectl flags...]   Pass through to kubectl exec for the selected Pod
	  export [-- flags...]      Stream result.jsonl to local disk
	  preheat [-- flags...]     Run preheat via daemon API
	  destroy                   Delete the prepared Pod and clear local state

	Global options:
	  --kubeconfig PATH   Path to kubeconfig (required)
	  --namespace NAME    Kubernetes namespace for the runner Pod
	  --name NAME         Pod name to operate on (required for pod-targeting subcommands)
	  --image IMAGE       Runner image (default: public buildctl-daemon image)
	  --image-pull-secret NAME  Docker config Secret in the runner namespace
	  --wait TIMEOUT      Timeout used by prepare when waiting for Pod readiness
	  -h, --help          Show this help

	Examples:
	  buildctl-batch --kubeconfig ./kubeconfig.yaml list
	  buildctl-batch --kubeconfig ./kubeconfig.yaml scale
	  buildctl-batch --kubeconfig ./kubeconfig.yaml scale --target 80
	  buildctl-batch --kubeconfig ./kubeconfig.yaml --name buildctl-batch-demo prepare
	  buildctl-batch --kubeconfig ./kubeconfig.yaml --name buildctl-batch-demo build --concurrency 4 --timeout 300 --retry 2 --oci --var BUILDCTL_BATCH_IMAGE_REPO=registry.example.com/project < ./source.zip
	  buildctl-batch --kubeconfig ./kubeconfig.yaml build --ttl 1h --concurrency 4 < ./source.zip
	  buildctl-batch --kubeconfig ./kubeconfig.yaml --name buildctl-batch-demo export --result ./result.jsonl --with-fail
	  buildctl-batch --kubeconfig ./kubeconfig.yaml --name buildctl-batch-demo preheat -- --dragonfly-scheduler-addr 10.0.0.1:8002

	Notes:
	  - The runner Pod executes buildctl-batch daemon on startup, exposing a UDS HTTP API.
	  - Go pprof is disabled by default. Set BUILDCTL_BATCH_DAEMON_PPROF_SERVER=127.0.0.1:6060 before prepare to enable it for port-forward debugging.
	  - build, export, and preheat communicate with the daemon via kubectl exec + curl.
	  - build automatically cancels the currently running batch before starting a new one.
	  - build --ttl X creates a one-shot Job that does not need prepare/destroy; X accepts s, m, h, or d (e.g. 90s, 30m, 1h). --name is optional and defaults to buildctl-batch-ttl-<suffix>. The Job self-deletes X after finishing via ttlSecondsAfterFinished.
	  - The default runner image is the public buildctl-daemon release image.
	  - The runner container must contain: buildctl-batch, curl, and buildctl.
	  - Set --image-pull-secret for a private runner image or target registry;
	    the Secret must exist in --namespace and contain .dockerconfigjson.
	EOF
}

die() {
	echo "error: $*" >&2
	exit 1
}

random_suffix() {
	openssl rand -hex 4 | cut -c 1-6
}

# Parse a duration like 90, 30s, 5m, 2h, 1d into whole seconds.
# Echoes the number of seconds on success; dies on invalid input.
parse_duration_seconds() {
	local input=$1
	[[ -n "${input}" ]] || die "duration must not be empty"

	local value=${input%[smhd]}
	local unit=${input: -1}
	local multiplier=1

	case "${unit}" in
	s) ;;
	m) multiplier=60 ;;
	h) multiplier=3600 ;;
	d) multiplier=86400 ;;
	[0-9])
		# No unit suffix; treat the whole input as seconds.
		value=${input}
		;;
	*)
		die "invalid duration unit in '${input}'; use s, m, h, or d"
		;;
	esac

	[[ "${value}" =~ ^[0-9]+$ ]] || die "invalid duration '${input}'; expected a number optionally followed by s, m, h, or d"
	echo $((value * multiplier))
}

kubectl_cmd() {
	local args=()
	[[ -z "${KUBECONFIG_PATH}" ]] || args+=(--kubeconfig "${KUBECONFIG_PATH}")
	[[ -z "${NAMESPACE}" ]] || args+=(--namespace "${NAMESPACE}")
	kubectl "${args[@]}" "$@"
}

kubectl_cmd_in_namespace() {
	local namespace=$1
	shift
	local args=()
	[[ -z "${KUBECONFIG_PATH}" ]] || args+=(--kubeconfig "${KUBECONFIG_PATH}")
	[[ -z "${namespace}" ]] || args+=(--namespace "${namespace}")
	kubectl "${args[@]}" "$@"
}

decode_base64_value() {
	local value=$1
	if printf '%s' "${value}" | base64 --decode >/dev/null 2>&1; then
		printf '%s' "${value}" | base64 --decode
		return
	fi
	if printf '%s' "${value}" | base64 -d >/dev/null 2>&1; then
		printf '%s' "${value}" | base64 -d
		return
	fi
	printf '%s' "${value}" | base64 -D
}

read_secret_data_field() {
	local namespace=$1
	local secret_name=$2
	local field_name=$3
	local encoded
	encoded=$(kubectl_cmd_in_namespace "${namespace}" get secret "${secret_name}" -o go-template="{{index .data \"${field_name}\"}}") || return 1
	[[ -n "${encoded}" ]] || return 1
	decode_base64_value "${encoded}"
}

resolve_runner_image() {
	if [[ -n "${RUNNER_IMAGE}" ]]; then
		return
	fi
	[[ -n "${IMAGE_PULL_SECRET}" ]] || die "runner image is empty; set --image or BUILDCTL_BATCH_IMAGE"
	RUNNER_IMAGE=$(read_secret_data_field "${NAMESPACE}" "${IMAGE_PULL_SECRET}" buildctl-batch-image) || die "failed to read buildctl-batch-image from ${NAMESPACE}/${IMAGE_PULL_SECRET}"
	[[ -n "${RUNNER_IMAGE}" ]] || die "buildctl-batch-image in ${NAMESPACE}/${IMAGE_PULL_SECRET} is empty"
}

save_state() {
	cat > "${STATE_FILE}" <<EOF
pod=${1}
namespace=${2}
EOF
}

require_pod_name() {
	[[ -n "${POD_NAME}" ]] || die "--name is required"
}

require_kubeconfig() {
	[[ -n "${KUBECONFIG_PATH}" ]] || die "--kubeconfig is required"
}

# Extract the value of a JSON string field (no jq dependency).
json_field() {
	local json=$1 key=$2
	echo "${json}" | sed -n 's/.*"'"${key}"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1
}

build_status_json() {
	require_pod_name
	kubectl_cmd exec "${POD_NAME}" -- \
		curl -sS --unix-socket "${DAEMON_SOCKET}" \
		http://localhost/api/v1/build/status
}

cancel_build_json() {
	require_pod_name
	kubectl_cmd exec "${POD_NAME}" -- \
		curl -sS -X POST --unix-socket "${DAEMON_SOCKET}" \
		http://localhost/api/v1/build/cancel
}

join_query_parts() {
	local IFS='&'
	echo "$*"
}

url_encode() {
	local LC_ALL=C input=$1 output="" char encoded
	local index
	for ((index = 0; index < ${#input}; index++)); do
		char=${input:index:1}
		case "${char}" in
		[a-zA-Z0-9.~_-]) output+="${char}" ;;
		*)
			printf -v encoded '%%%02X' "'${char}"
			output+="${encoded}"
			;;
		esac
	done
	printf '%s' "${output}"
}

query_part() {
	printf '%s=%s' "$(url_encode "$1")" "$(url_encode "$2")"
}

stream_upload_input() {
	if [[ ! -t 2 ]]; then
		cat
		return
	fi

	if command -v pv >/dev/null 2>&1; then
		pv -ptebar
		return
	fi

	if dd --help 2>/dev/null | grep -q 'status='; then
		dd bs=1M status=progress
		return
	fi

	cat
}

render_prepare_manifest() {
	local pod_name=$1
	local namespace=$2
	cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_name}
  namespace: ${namespace}
  labels:
    app.kubernetes.io/name: buildctl-batch-runner
spec:
EOF
	render_runner_pod_spec | indent_lines "  "
}

# render_job_manifest renders a batch/v1 Job whose Pod template runs the
# buildctl-batch daemon. ttlSecondsAfterFinished lets Kubernetes delete the Job
# (and its Pod) automatically after it finishes.
render_job_manifest() {
	local job_name=$1
	local namespace=$2
	local ttl_seconds=$3
	cat <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job_name}
  namespace: ${namespace}
  labels:
    app.kubernetes.io/name: buildctl-batch-runner
spec:
  ttlSecondsAfterFinished: ${ttl_seconds}
  backoffLimit: 0
  template:
    metadata:
      labels:
        app.kubernetes.io/name: buildctl-batch-runner
    spec:
EOF
	render_runner_pod_spec | indent_lines "      "
}

# indent_lines prefixes every line of stdin with the given string.
indent_lines() {
	local prefix=$1
	sed "s/^/${prefix}/"
}

# render_runner_pod_spec emits the Pod .spec body (restartPolicy, optional
# imagePullSecrets, the runner container, and optional docker-config volume) at
# zero base indentation. Callers add indentation via indent_lines so the same
# block can be reused for both a bare Pod and a Job Pod template.
render_runner_pod_spec() {
	cat <<EOF
restartPolicy: Never
EOF
	if [[ -n "${IMAGE_PULL_SECRET}" ]]; then
		cat <<EOF
imagePullSecrets:
  - name: ${IMAGE_PULL_SECRET}
EOF
	fi
	cat <<EOF
containers:
  - name: runner
    image: ${RUNNER_IMAGE}
    imagePullPolicy: Always
    command:
      - buildctl-batch
      - daemon
      - --addrs
      - "${BUILDKIT_SERVICE_ADDR}"
    resources:
      requests:
        cpu: "500m"
        memory: "512Mi"
      limits:
        cpu: "2"
        memory: "2Gi"
EOF
	if [[ -n "${DAEMON_PPROF_SERVER}" ]]; then
		cat <<EOF
    env:
      - name: BUILDCTL_BATCH_PPROF_SERVER
        value: "${DAEMON_PPROF_SERVER}"
EOF
	fi
	if [[ -n "${IMAGE_PULL_SECRET}" ]]; then
		cat <<EOF
    volumeMounts:
      - name: docker-config
        mountPath: /root/.docker
        readOnly: true
volumes:
  - name: docker-config
    secret:
      secretName: ${IMAGE_PULL_SECRET}
      items:
        - key: .dockerconfigjson
          path: config.json
EOF
	fi
}

build_query_from_args() {
	BUILD_QUERY=""
	TTL_SECONDS=""
	local parts=()
	local target=""
	local key
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--image-dirs|--image-dirs=*|--addrs|--addrs=*|--result|--result=*|--logs|--logs=*)
			die "build manages $1 automatically"
			;;
		--registry|--registry=*)
			die "build no longer supports $1; use --var BUILDCTL_BATCH_*=..."
			;;
		--ttl)
			[[ $# -ge 2 ]] || die "--ttl requires a value"
			TTL_SECONDS=$(parse_duration_seconds "$2")
			shift 2
			;;
		--ttl=*)
			TTL_SECONDS=$(parse_duration_seconds "${1#--ttl=}")
			shift
			;;
		--var)
			[[ $# -ge 2 ]] || die "--var requires a value"
			parts+=("$(query_part var "$2")")
			shift 2
			;;
		--var=*)
			parts+=("$(query_part var "${1#--var=}")")
			shift
			;;
		--fail-fast|--oci|--both-formats|--skip-fail|--verbose)
			parts+=("$(query_part "${1#--}" true)")
			shift
			;;
		--concurrency|--oom-cooldown|--timeout|--retry)
			[[ $# -ge 2 ]] || die "$1 requires a value"
			parts+=("$(query_part "${1#--}" "$2")")
			shift 2
			;;
		--concurrency=*|--oom-cooldown=*|--timeout=*|--retry=*)
			key=${1%%=*}
			parts+=("$(query_part "${key#--}" "${1#*=}")")
			shift
			;;
		--*)
			die "unknown build flag: $1"
			;;
		*)
			if [[ -n "${target}" ]]; then
				die "build accepts at most one positional target"
			fi
			target=$1
			shift
			;;
		esac
	done
	if [[ -n "${target}" ]]; then
		parts+=("$(query_part target "${target}")")
	fi
	BUILD_QUERY=$(join_query_parts "${parts[@]}")
}

export_query_from_args() {
	EXPORT_QUERY=""
	LOCAL_EXPORT_RESULT_PATH="result.jsonl"
	local parts=()
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--result)
			[[ $# -ge 2 ]] || die "--result requires a value"
			LOCAL_EXPORT_RESULT_PATH=$2
			shift 2
			;;
		--result=*)
			LOCAL_EXPORT_RESULT_PATH=${1#--result=}
			shift
			;;
		--from-result|--from-result=*)
			die "export manages $1 automatically"
			;;
		--oci|--with-fail)
			parts+=("$(query_part "${1#--}" true)")
			shift
			;;
		--*)
			die "unknown export flag: $1"
			;;
		*)
			die "export does not accept positional arguments: $1"
			;;
		esac
	done
	[[ -n "${LOCAL_EXPORT_RESULT_PATH}" ]] || die "local export result path must not be empty"
	EXPORT_QUERY=$(join_query_parts "${parts[@]}")
}

preheat_query_from_args() {
	PREHEAT_QUERY=""
	local parts=()
	local key
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--from-result|--from-result=*)
			die "preheat manages $1 automatically"
			;;
		--fail-fast|--oci|--verbose)
			parts+=("$(query_part "${1#--}" true)")
			shift
			;;
		--dragonfly-scheduler-addr|--concurrency|--interval|--timeout)
			[[ $# -ge 2 ]] || die "$1 requires a value"
			parts+=("$(query_part "${1#--}" "$2")")
			shift 2
			;;
		--dragonfly-scheduler-addr=*|--concurrency=*|--interval=*|--timeout=*)
			key=${1%%=*}
			parts+=("$(query_part "${key#--}" "${1#*=}")")
			shift
			;;
		--*)
			die "unknown preheat flag: $1"
			;;
		*)
			die "preheat does not accept positional arguments: $1"
			;;
		esac
	done
	PREHEAT_QUERY=$(join_query_parts "${parts[@]}")
}

# ---- prepare ----

# wait_daemon_ready polls the daemon health endpoint inside the given Pod until
# it responds or DAEMON_READY_TIMEOUT_SECONDS elapses. Returns non-zero on
# timeout.
wait_daemon_ready() {
	local pod_name=$1
	local i
	for i in $(seq 1 "${DAEMON_READY_TIMEOUT_SECONDS}"); do
		if kubectl_cmd exec "${pod_name}" -- \
			curl -sS --max-time 2 --unix-socket "${DAEMON_SOCKET}" \
			http://localhost/api/v1/health >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	return 1
}

prepare_runner_pod() {
	local pod_name=${POD_NAME}
	resolve_runner_image
	if kubectl_cmd get pod "${pod_name}" >/dev/null 2>&1; then
		rm -f "${STATE_FILE}"
		die "pod ${pod_name} already exists in namespace ${NAMESPACE}"
	fi
	render_prepare_manifest "${pod_name}" "${NAMESPACE}" | kubectl_cmd create -f -
	kubectl_cmd wait --for=condition=Ready "pod/${pod_name}" --timeout "${WAIT_TIMEOUT}"

	# Wait for daemon to be ready on the UDS socket
	if ! wait_daemon_ready "${pod_name}"; then
		kubectl_cmd logs "${pod_name}" --tail=50 >&2 || true
		die "daemon did not become ready within ${DAEMON_READY_TIMEOUT_SECONDS}s"
	fi

	save_state "${pod_name}" "${NAMESPACE}"
	echo "${pod_name}"
}

destroy_runner_pod() {
	require_pod_name
	kubectl_cmd delete pod "${POD_NAME}" --ignore-not-found=true --wait=true
	if [[ -f "${STATE_FILE}" ]]; then
		local saved_pod=""
		local saved_ns=""
		while IFS='=' read -r key value; do
			case "${key}" in
			pod)
				saved_pod=${value}
				;;
			namespace)
				saved_ns=${value}
				;;
			esac
		done < "${STATE_FILE}"
		if [[ "${saved_pod}" == "${POD_NAME}" && "${saved_ns}" == "${NAMESPACE}" ]]; then
			rm -f "${STATE_FILE}"
		fi
	fi
}

# ---- build ----

run_build() {
	[[ ! -t 0 ]] || die "build requires a .zip stream on stdin"

	local query
	local status_response
	local status
	build_query_from_args "$@"

	if [[ -n "${TTL_SECONDS}" ]]; then
		run_build_ttl
		return
	fi

	require_pod_name
	status_response=$(build_status_json)
	status=$(json_field "${status_response}" "status")
	if [[ "${status}" == "running" ]]; then
		echo "Cancelling existing build before starting a new one" >&2
		cancel_build_json >/dev/null
		while true; do
			status_response=$(build_status_json)
			status=$(json_field "${status_response}" "status")
			if [[ "${status}" != "running" ]]; then
				break
			fi
			sleep "${WAIT_BUILD_INTERVAL_SECONDS}"
		done
	fi
	query=$(query_part addrs "${BUILDKIT_SERVICE_ADDR}")
	[[ -z "${BUILD_QUERY}" ]] || query="${query}&${BUILD_QUERY}"

	# Upload zip and return once daemon accepts the async build request.
	local response
	response=$(stream_upload_input | kubectl_cmd exec -i "${POD_NAME}" -- \
		curl -sS -X POST -T - \
		--unix-socket "${DAEMON_SOCKET}" \
		"http://localhost/api/v1/build?${query}")
	echo "${response}" >&2

	local build_status
	build_status=$(json_field "${response}" "status")
	if [[ "${build_status}" == "failed" || "${build_status}" == "error" ]]; then
		return 1
	fi
}

# run_build_ttl runs the one-shot Job mode: it creates a self-cleaning Job (no
# prepare required), uploads the zip, requests a oneshot build, then exits
# without waiting. The daemon shuts itself down once the build finishes, the Job
# completes, and ttlSecondsAfterFinished deletes the Job and its Pod after the
# configured TTL.
run_build_ttl() {
	require_kubeconfig
	resolve_runner_image

	local job_name="${POD_NAME}"
	if [[ -z "${job_name}" ]]; then
		job_name="buildctl-batch-ttl-$(random_suffix)"
	fi

	if kubectl_cmd get job "${job_name}" >/dev/null 2>&1; then
		die "job ${job_name} already exists in namespace ${NAMESPACE}"
	fi

	echo "Creating one-shot Job ${job_name} (ttlSecondsAfterFinished=${TTL_SECONDS}s) in namespace ${NAMESPACE}" >&2
	render_job_manifest "${job_name}" "${NAMESPACE}" "${TTL_SECONDS}" | kubectl_cmd create -f -

	# Resolve the Pod created by the Job and wait until it is ready.
	kubectl_cmd wait --for=condition=Ready pod -l "job-name=${job_name}" --timeout "${WAIT_TIMEOUT}"
	local pod_name
	pod_name=$(kubectl_cmd get pods -l "job-name=${job_name}" -o jsonpath='{.items[0].metadata.name}')
	[[ -n "${pod_name}" ]] || die "failed to resolve Pod for job ${job_name}"

	if ! wait_daemon_ready "${pod_name}"; then
		kubectl_cmd logs "${pod_name}" --tail=50 >&2 || true
		die "daemon did not become ready within ${DAEMON_READY_TIMEOUT_SECONDS}s"
	fi

	local query
	query="$(query_part addrs "${BUILDKIT_SERVICE_ADDR}")&$(query_part oneshot true)"
	[[ -z "${BUILD_QUERY}" ]] || query="${query}&${BUILD_QUERY}"

	# Upload zip and request the oneshot build. The daemon runs the build and
	# then exits on its own; we do not wait for completion here.
	local response
	response=$(stream_upload_input | kubectl_cmd exec -i "${pod_name}" -- \
		curl -sS -X POST -T - \
		--unix-socket "${DAEMON_SOCKET}" \
		"http://localhost/api/v1/build?${query}")
	echo "${response}" >&2

	local build_status
	build_status=$(json_field "${response}" "status")
	if [[ "${build_status}" == "failed" || "${build_status}" == "error" ]]; then
		return 1
	fi

	echo "Job ${job_name} accepted the build; it will self-delete ${TTL_SECONDS}s after finishing." >&2
	echo "Note: results are not exported in one-shot mode; verify the built images in the target registry." >&2
	echo "Track it with: kubectl get job ${job_name} -n ${NAMESPACE}" >&2
}

run_status() {
	local response
	response=$(build_status_json)
	echo "${response}"
}

run_list() {
	if [[ $# -eq 0 ]]; then
		kubectl_cmd get pods -l app.kubernetes.io/name=buildctl-batch-runner
		return
	fi
	kubectl_cmd get pods "$@"
}

run_scale() {
	local target=""

	while [[ $# -gt 0 ]]; do
		case "$1" in
		--target)
			[[ $# -ge 2 ]] || die "--target requires a value"
			target=$2
			shift 2
			;;
		--target=*)
			target=${1#--target=}
			shift
			;;
		*)
			die "unknown scale flag: $1"
			;;
		esac
	done

	if [[ -z "${target}" ]]; then
		kubectl_cmd_in_namespace "${BUILDKIT_SERVICE_NAMESPACE}" get pods \
			-l app.kubernetes.io/component=buildkitd -o wide
		return
	fi

	[[ "${target}" =~ ^[0-9]+$ ]] || die "--target must be a non-negative integer"
	kubectl_cmd_in_namespace "${BUILDKIT_SERVICE_NAMESPACE}" scale "deployment/${BUILDKIT_SERVICE_DEPLOYMENT}" --replicas="${target}"
}

run_wait() {
	require_pod_name
	local timeout_seconds=0
	local interval_seconds="${WAIT_BUILD_INTERVAL_SECONDS}"

	while [[ $# -gt 0 ]]; do
		case "$1" in
		--timeout)
			[[ $# -ge 2 ]] || die "--timeout requires a value"
			timeout_seconds=$2
			shift 2
			;;
		--timeout=*)
			timeout_seconds=${1#--timeout=}
			shift
			;;
		--interval)
			[[ $# -ge 2 ]] || die "--interval requires a value"
			interval_seconds=$2
			shift 2
			;;
		--interval=*)
			interval_seconds=${1#--interval=}
			shift
			;;
		*)
			die "unknown wait flag: $1"
			;;
		esac
	done

	[[ "${timeout_seconds}" =~ ^[0-9]+$ ]] || die "--timeout must be a non-negative integer"
	[[ "${interval_seconds}" =~ ^[0-9]+$ ]] || die "--interval must be a positive integer"
	[[ "${interval_seconds}" -ge 1 ]] || die "--interval must be greater than or equal to 1"

	local start_ts=$SECONDS
	while true; do
		local response
		local status
		response=$(build_status_json)
		status=$(json_field "${response}" "status")
		echo "${response}"
		case "${status}" in
		running)
			if [[ "${timeout_seconds}" -gt 0 && $((SECONDS - start_ts)) -ge "${timeout_seconds}" ]]; then
				die "wait timed out after ${timeout_seconds}s"
			fi
			sleep "${interval_seconds}"
			;;
		failed|error)
			return 1
			;;
		completed)
			return 0
			;;
		idle)
			die "no build is running; wait requires an active or completed build"
			;;
		"")
			die "failed to parse build status from daemon response"
			;;
		*)
			die "unknown build status: ${status}"
			;;
		esac
	done
}

run_logs() {
	require_pod_name
	kubectl_cmd logs "${POD_NAME}" "$@"
}

run_exec() {
	require_pod_name
	kubectl_cmd exec "${POD_NAME}" "$@"
}

# ---- export ----

run_export() {
	require_pod_name

	local local_result="result.jsonl"
	local query=""
	export_query_from_args "$@"
	query=${EXPORT_QUERY}
	local_result=${LOCAL_EXPORT_RESULT_PATH}

	local local_dir
	local_dir=$(dirname -- "${local_result}")
	mkdir -p "${local_dir}"

	local tmp_output
	tmp_output=$(mktemp "${local_result}.tmp.XXXXXX")

	if ! kubectl_cmd exec "${POD_NAME}" -- sh -lc '
		set -eu
		socket=$1
		url=$2
		body=$(mktemp)
		cleanup() {
			rm -f "$body"
		}
		trap cleanup EXIT
		code=$(curl -sS -o "$body" -w "%{http_code}" -X POST --unix-socket "$socket" "$url")
		if [ "$code" -lt 200 ] || [ "$code" -ge 300 ]; then
			cat "$body" >&2
			exit 1
		fi
		cat "$body"
	' sh "${DAEMON_SOCKET}" "http://localhost/api/v1/export?${query}" > "${tmp_output}"; then
		rm -f "${tmp_output}"
		die "export request failed"
	fi

	mv "${tmp_output}" "${local_result}"
	echo "Exported to ${local_result}" >&2
}

# ---- preheat ----

run_preheat() {
	require_pod_name

	local query=""
	preheat_query_from_args "$@"
	query=${PREHEAT_QUERY}

	kubectl_cmd exec "${POD_NAME}" -- sh -lc '
		set -eu
		socket=$1
		url=$2
		body=$(mktemp)
		trap '\''rm -f "$body"'\'' EXIT
		code=$(curl -sS -o "$body" -w "%{http_code}" -X POST --unix-socket "$socket" "$url")
		cat "$body"
		[ "$code" -ge 200 ] && [ "$code" -lt 300 ]
	' sh "${DAEMON_SOCKET}" "http://localhost/api/v1/preheat?${query}"
}

# ---- global flag parsing ----

parse_global_flags() {
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--kubeconfig)
			[[ $# -ge 2 ]] || die "--kubeconfig requires a value"
			KUBECONFIG_PATH=$2
			shift 2
			;;
		--namespace)
			[[ $# -ge 2 ]] || die "--namespace requires a value"
			NAMESPACE=$2
			shift 2
			;;
		--name)
			[[ $# -ge 2 ]] || die "--name requires a value"
			POD_NAME=$2
			shift 2
			;;
		--image)
			[[ $# -ge 2 ]] || die "--image requires a value"
			RUNNER_IMAGE=$2
			shift 2
			;;
		--image-pull-secret)
			[[ $# -ge 2 ]] || die "--image-pull-secret requires a value"
			IMAGE_PULL_SECRET=$2
			shift 2
			;;
		--wait)
			[[ $# -ge 2 ]] || die "--wait requires a value"
			WAIT_TIMEOUT=$2
			shift 2
			;;
		-h|--help)
			usage
			exit 0
			;;
		--)
			shift
			break
			;;
		-*)
			die "unknown global flag: $1"
			;;
		*)
			break
			;;
		esac
	done

	REMAINING_ARGS=("$@")
}

# ---- main ----

main() {
	parse_global_flags "$@"
	set -- "${REMAINING_ARGS[@]}"
	[[ $# -ge 1 ]] || {
		usage
		exit 1
	}

	local subcommand=$1
	shift

	case "${subcommand}" in
	prepare)
		require_kubeconfig
		require_pod_name
		while [[ $# -gt 0 ]]; do
			case "$1" in
			--image)
				[[ $# -ge 2 ]] || die "--image requires a value"
				RUNNER_IMAGE=$2
				shift 2
				;;
			--image-pull-secret)
				[[ $# -ge 2 ]] || die "--image-pull-secret requires a value"
				IMAGE_PULL_SECRET=$2
				shift 2
				;;
			*)
				die "unknown prepare flag: $1"
				;;
			esac
		done
		prepare_runner_pod
		;;
	list)
		require_kubeconfig
		if [[ $# -gt 0 && "$1" == "--" ]]; then shift; fi
		run_list "$@"
		;;
	scale)
		require_kubeconfig
		if [[ $# -gt 0 && "$1" == "--" ]]; then shift; fi
		run_scale "$@"
		;;
	build)
		require_kubeconfig
		if [[ $# -gt 0 && "$1" == "--" ]]; then shift; fi
		run_build "$@"
		;;
	status)
		require_kubeconfig
		run_status
		;;
	wait)
		require_kubeconfig
		if [[ $# -gt 0 && "$1" == "--" ]]; then shift; fi
		run_wait "$@"
		;;
	logs)
		require_kubeconfig
		if [[ $# -gt 0 && "$1" == "--" ]]; then shift; fi
		run_logs "$@"
		;;
	exec)
		require_kubeconfig
		if [[ $# -gt 0 && "$1" == "--" ]]; then shift; fi
		run_exec "$@"
		;;
	export)
		require_kubeconfig
		if [[ $# -gt 0 && "$1" == "--" ]]; then shift; fi
		run_export "$@"
		;;
	preheat)
		require_kubeconfig
		if [[ $# -gt 0 && "$1" == "--" ]]; then shift; fi
		run_preheat "$@"
		;;
	destroy)
		require_kubeconfig
		destroy_runner_pod
		;;
	-h|--help|help)
		usage
		;;
	*)
		die "unknown subcommand: ${subcommand}"
		;;
	esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	main "$@"
fi
