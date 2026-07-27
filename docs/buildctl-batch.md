# Batch Build CLI (`buildctl-batch`)

`buildctl-batch` builds batches of Dockerfiles into OCI / Nydus images and pushes
them to a target registry. It can also preheat built images into a
[Dragonfly](https://d7y.io) P2P cluster (see [preheat](preheat.md)).

The batch `preheat` subcommand is intended for public registries on a trusted
cluster network: it verifies Registry TLS but uses the Dragonfly Scheduler's
plaintext in-cluster endpoint and sends no Registry credentials. For private
registries or Scheduler TLS, use [scripts/preheat.sh](preheat.md).

- **runner**: the pod that drives a build batch; use one runner per batch.
- **worker**: a buildkitd pod that executes builds. A runner spreads its builds
  across workers, which can be scaled with demand.

> To submit a single build over HTTP, use [buildctl-daemon](buildctl-daemon.md). To
> speed up dependency installation inside Dockerfiles, use
> [package-mirror](package-mirror.md).

## Building / converting images

### Prerequisites

- [kubectl](https://kubernetes.io/docs/tasks/tools/) installed and on `$PATH`
  (Linux recommended), plus a kubeconfig for the build cluster.
- A deployed buildkit-service worker pool; use the repository
  [quick start](../README.md#quick-start) for a small evaluation cluster.
- Base images referenced by your Dockerfiles must already exist in a registry
  reachable from the cluster.

The wrapper starts its runner from the public
`ghcr.io/inclusionai/buildkit-service/buildctl-daemon:latest` image. For a
private runner image or authenticated source/target registries, create a Docker
config Secret in the runner namespace and pass it to `prepare`:

```bash
kubectl -n default create secret generic buildctl-registry \
  --type=kubernetes.io/dockerconfigjson \
  --from-file=.dockerconfigjson="$HOME/.docker/config.json"

./scripts/buildctl-batch.sh --kubeconfig kubeconfig.yaml \
  --name my-batch-task-1 prepare \
  --image-pull-secret buildctl-registry
```

### 1. Prepare the Dockerfile context archive

Create a `source.zip` with the following layout:

```text
├── image-1              # any directory name; each directory is one Dockerfile context
│   ├── Dockerfile       # required, standard Dockerfile
│   ├── metadata.json    # required, format: { "target": "<target image name>" }
│   └── ...              # optional, other files/directories needed by the build
├── image-2
│   ├── Dockerfile
│   ├── metadata.json
│   └── ...
└── ...
```

Example `Dockerfile`:

```dockerfile
FROM $BUILDCTL_BATCH_IMAGE_REGISTRY/myorg/image:ubuntu-v0.1.1
...
```

Example `metadata.json`:

```json
{
  "target": "$BUILDCTL_BATCH_IMAGE_REGISTRY/myorg/image:ubuntu-built-v0.1.1"
}
```

Notes:

- `Dockerfile` and `metadata.json` may use `$BUILDCTL_BATCH_*`-style variables
  (`$BUILDCTL_BATCH_IMAGE_REGISTRY` or `${BUILDCTL_BATCH_IMAGE_REGISTRY}`),
  substituted at build time via repeatable `--var KEY=value` flags.
- Image names are `host/namespace/repo:tag`. Keep the `host/namespace/repo`
  prefix fixed and distinguish large image sets by `tag`; many registries limit
  the number of repositories, and tags must not exceed 110 characters.
- Legacy shell heredocs in Dockerfiles (`RUN cat > /path <<EOF`) are
  automatically converted server-side to native `COPY <<EOF`; see the
  [heredoc notes in buildctl-daemon.md](buildctl-daemon.md).
- The runner rejects archives larger than 512 MiB compressed, 4 GiB total
  uncompressed, or 100,000 entries.

### 2. Build images

```bash
BUILDCTL_KUBECONFIG=kubeconfig.yaml

# Pick a task name
BUILDCTL_BATCH_NAME=my-batch-task-1

# Prepare the build environment; use one per batch, --name is required
./scripts/buildctl-batch.sh --kubeconfig $BUILDCTL_KUBECONFIG \
  --name $BUILDCTL_BATCH_NAME \
  prepare

# Try a small run first; source.zip is passed on stdin
./scripts/buildctl-batch.sh --kubeconfig $BUILDCTL_KUBECONFIG \
  --name $BUILDCTL_BATCH_NAME \
  build --timeout 86400 --concurrency 1 --fail-fast --verbose < source.zip

# More options:
#
# --verbose        # default false; print detailed build logs (debug-friendly)
# --fail-fast      # default false; stop at the first failed task (debug-friendly)
# --oci            # default false; build OCI images instead of Nydus images
# --both-formats   # default false; build Nydus then OCI for each target on the
#                  # same worker; the task only succeeds when both formats
#                  # succeed. --concurrency still counts targets.
# --skip-fail      # default false; skip tasks that failed in previous runs
# --retry N        # retries per failed task
# --concurrency    # maximum concurrent build tasks
# --ttl X          # default empty. Enables "one-shot Job mode": no prepare
#                  # needed; creates a self-cleaning Job and exits immediately
#                  # after submission. When the build finishes the daemon exits
#                  # gracefully and Kubernetes deletes the Job and Pod after X
#                  # (ttlSecondsAfterFinished). X accepts s/m/h/d (90s, 30m,
#                  # 1h). --name is optional (auto-generated
#                  # buildctl-batch-ttl-<random> by default). This mode does not
#                  # return results; verify artifacts in the target registry.

# Full batch build with concurrency 20
./scripts/buildctl-batch.sh --kubeconfig $BUILDCTL_KUBECONFIG \
  --name $BUILDCTL_BATCH_NAME \
  build --timeout 86400 --retry 10 --concurrency 20 < source.zip

# One-shot Job mode: no prepare/destroy, exits right after submission,
# the Job cleans itself up 1 hour after completion
./scripts/buildctl-batch.sh --kubeconfig $BUILDCTL_KUBECONFIG \
  build --ttl 1h --timeout 86400 --concurrency 20 < source.zip

# Private runner/source/target registry in one-shot mode. The global Secret
# option must appear before the build subcommand.
./scripts/buildctl-batch.sh --kubeconfig $BUILDCTL_KUBECONFIG \
  --image-pull-secret buildctl-registry \
  build --ttl 1h --timeout 86400 --concurrency 20 < source.zip
```

The wrapper defaults to the release and namespace names used in this guide.
Override these variables when the Helm installation uses different names:

| Variable | Default | Purpose |
| --- | --- | --- |
| `BUILDCTL_BATCH_BUILDKIT_ADDR` | `tcp://buildkit-service.buildkit-service.svc:9094` | BuildKit Service address used by runner Pods. |
| `BUILDCTL_BATCH_SERVICE_NAMESPACE` | `buildkit-service` | Namespace used by `scale`. |
| `BUILDCTL_BATCH_SERVICE_DEPLOYMENT` | `buildkit-service` | BuildKit Deployment used by `scale`. |
| `BUILDCTL_BATCH_IMAGE` | public release image | Override the runner image. |
| `BUILDCTL_BATCH_IMAGE_PULL_SECRET` | empty | Docker config Secret for one-shot mode or automation. |

### 3. Converting images

The same flow converts existing OCI images to Nydus images: write a Dockerfile
containing a single `FROM <source OCI image>` line. `FROM` a Nydus image is not
supported yet.

### 4. Tracking tasks and exporting results

```bash
# List all runner pods
./scripts/buildctl-batch.sh --kubeconfig $BUILDCTL_KUBECONFIG \
  list

# Stream build logs; extra flags pass through to kubectl logs
./scripts/buildctl-batch.sh --kubeconfig $BUILDCTL_KUBECONFIG \
  --name $BUILDCTL_BATCH_NAME \
  logs --tail 100 -f

# Exec into the runner pod and inspect the latest failed task
./scripts/buildctl-batch.sh --kubeconfig $BUILDCTL_KUBECONFIG \
  --name $BUILDCTL_BATCH_NAME \
  exec -it -- sh
tail -n 1 /tmp/logs.jsonl

# Export currently-succeeded tasks from the result store
./scripts/buildctl-batch.sh --kubeconfig $BUILDCTL_KUBECONFIG \
  --name $BUILDCTL_BATCH_NAME \
  export

# Generates a local result.jsonl file:
# {"target":"","success":true}

# More options:
#
# --oci        # default false; only export OCI tasks
# --with-fail  # default false; also export failed tasks

# Wait for the current build to finish; exit code 0 only for completed,
# non-zero for failed/error. Errors immediately when idle or the status
# cannot be parsed.
./scripts/buildctl-batch.sh --kubeconfig $BUILDCTL_KUBECONFIG \
  --name $BUILDCTL_BATCH_NAME \
  wait --timeout 3600
```

### 5. Deleting a task

```bash
./scripts/buildctl-batch.sh --kubeconfig $BUILDCTL_KUBECONFIG \
  --name $BUILDCTL_BATCH_NAME \
  destroy
```

### 6. Scaling workers

```bash
# Show current worker pod count and status
./scripts/buildctl-batch.sh --kubeconfig $BUILDCTL_KUBECONFIG \
  scale

# Scale workers to 20. If the cluster has node auto-scaling, new nodes may take
# a while to come up; use `scale` to watch progress.
# When done, lower --target (for example 3) to save cost.
./scripts/buildctl-batch.sh --kubeconfig $BUILDCTL_KUBECONFIG \
  scale --target 20
```

## BuildKit registry mirror

Dependency-install acceleration (pip/npm/apt/yum/apk/git) is covered by
[package-mirror](package-mirror.md). This section covers `FROM` image pulls.

Dockerfile `FROM` pulls are executed by buildkitd, so registry mirrors must be
configured in buildkitd's `buildkitd.toml`, not on `buildctl`. The Helm chart
can generate and mount this config:

```yaml
buildkitdConfig:
  enabled: true
```

The default configuration covers Docker Hub. Other registries declared in the
`registries` section get a pull-through cache when `mirror.always: true` (or
when they host a chart image). With mirrors enabled, Dockerfiles
keep their original `FROM` lines; BuildKit prefers the in-cluster
package-mirror registry cache and falls back to the origin registry when the
mirror is unavailable. For example:

```yaml
registries:
  - name: example
    host: registry.example.com
    remoteUrl: https://registry.example.com
    mirror:
      serviceName: registry-example
      enabled: true
      always: true
```

## Recommended node pools

Production sizing guidance for capacity planning (not identical to the chart
defaults). Core principle: BuildKit workers own their local cache and I/O, and
the package-mirror cache services are isolated in a separate node pool so cache
growth, OOMs, or disk eviction cannot affect builds.

| Node pool | Nodes | Per-node spec | Runs |
| --- | --- | --- | --- |
| `buildkitd` | 20 | 16 vCPU / 64 GiB / 2 TiB local disk | 20 buildkitd workers, ideally 1 worker/node |
| `package-mirror` | 14 | 32 vCPU / 64 GiB / 2 TiB local disk | pip / npm / apt-yum / apk / git / registry mirror |

### buildkitd node pool

Label/taint the pool (for example `owner=buildkitd`) so buildkitd workers land
there, ideally one worker per node. Each buildkitd keeps an independent
`/var/lib/buildkit` cache with a GC high-water target of roughly 400 GiB per
worker, but the node disk must also hold containerd image layers, snapshots,
build temporary files, logs, and GC lag. Keep substantial headroom above the
configured cache and emptyDir limits.

`buildctl-daemon` and the metrics aggregator are control-plane components and
do not need dedicated large nodes. The daemon currently keeps task state and
logs in-process/on local disk; multi-replica operation first requires shared
task state, log storage, and sticky load balancing.

### package-mirror node pool

Label/taint the pool (for example `owner=package-mirror`) to host only the
package caches. Ideal shape: 20 main `package-mirror` replicas serving
pip/npm/apt-yum/apk, 8 standalone git-cache replicas, and the registry mirrors
in the same pool. A main replica uses roughly pip 30 GiB + npm 80 GiB +
apt-yum 160 GiB (~270 GiB) of local cache quota; budget a git-cache replica at
200 GiB local cache and 16 GiB memory.

To reduce interference further, split into `package-mirror-main` (10 × 32 vCPU
/ 64 GiB / 2 TiB) and `package-mirror-git` (4 × 16 vCPU / 64 GiB / 1 TiB)
pools. The registry mirrors benefit most from dedicated per-replica PVCs (see
`packageMirror.registry.persistence` in the chart).

## Deployment and debugging

### Building and pushing images

Component images are published automatically by GitHub Actions (see
`.github/workflows/release.yml`). To build manually:

```bash
docker build -f chart/images/buildkit/Dockerfile \
  -t <registry>/<org>/buildctl-daemon:<tag> .
docker push <registry>/<org>/buildctl-daemon:<tag>
```

The package-mirror helper images (apt-cacher-ng, git-cache) are built from
`chart/images/`; see [package-mirror.md](package-mirror.md).

### Two Helm releases

`chart/` contains both the buildkit-service and package-mirror workloads. In
production, install them as two Helm releases with independent lifecycles:

- `buildkit-service` release: buildkitd, buildctl-daemon, metrics-aggregator,
  and the buildkitd registry mirror config.
- `package-mirror` release: pip/npm/apt-yum/apk/git/registry mirror caches.

```bash
TOKEN="$(openssl rand -hex 32)"

# 1. Install/upgrade package-mirror (BuildKit-side workloads disabled)
helm --kubeconfig $BUILDCTL_KUBECONFIG \
  upgrade --install package-mirror ./chart \
  --namespace package-mirror \
  --create-namespace \
  --set buildkit.enabled=false \
  --set packageMirror.enabled=true \
  --set packageMirror.namespaceOverride=package-mirror

# 2. Install/upgrade buildkit-service (package-mirror workloads disabled)
helm --kubeconfig $BUILDCTL_KUBECONFIG \
  upgrade --install buildkit-service ./chart \
  --namespace buildkit-service \
  --create-namespace \
  --set buildkit.enabled=true \
  --set packageMirror.enabled=false \
  --set-string buildctlDaemon.auth.token="$TOKEN"
```

Helm upgrades that change the token automatically roll the daemon Pod because
the token checksum is part of its PodTemplate. Keep `TOKEN` available to API
clients; generating a different value without upgrading the release has no
effect.

Diff before upgrading:

```bash
helm --kubeconfig $BUILDCTL_KUBECONFIG \
  template package-mirror ./chart \
  --namespace package-mirror \
  --set buildkit.enabled=false \
  --set packageMirror.enabled=true \
  --set packageMirror.namespaceOverride=package-mirror \
  | kubectl --kubeconfig $BUILDCTL_KUBECONFIG diff -f -

helm --kubeconfig $BUILDCTL_KUBECONFIG \
  template buildkit-service ./chart \
  --namespace buildkit-service \
  --set buildkit.enabled=true \
  --set packageMirror.enabled=false \
  --set-string buildctlDaemon.auth.token="$TOKEN" \
  | kubectl --kubeconfig $BUILDCTL_KUBECONFIG diff -f -
```

Notes:

- Install the package-mirror release first: buildkitd's registry mirror config
  references `registry-*.package-mirror.svc.cluster.local`.
- Both releases share `chart/values.yaml` but are trimmed via
  `buildkit.enabled` / `packageMirror.enabled`.
- Keep `packageMirror.namespaceOverride=package-mirror` explicit to avoid
  confusion when the release namespace differs from the target namespace.
- To upgrade only the buildkit image or buildctl-daemon, run only the second
  command; to change only the caches, run only the first.

### Debugging

```bash
# Forward the runner pod's pprof port for hang analysis
export BUILDCTL_BATCH_DAEMON_PPROF_SERVER=127.0.0.1:6060
# Run prepare after setting the variable, then:
kubectl --kubeconfig $BUILDCTL_KUBECONFIG -n default port-forward pod/$BUILDCTL_BATCH_NAME 6060:6060

# Grab goroutine stacks
curl -s http://127.0.0.1:6060/debug/pprof/goroutine?debug=2 > goroutines.txt
```

buildctl-daemon deployment flags, public LoadBalancer exposure, and pprof
debugging are covered in [buildctl-daemon.md](buildctl-daemon.md).
