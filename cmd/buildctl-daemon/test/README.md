# buildctl-daemon API smoke test

This directory contains a minimal Dockerfile and an automated integration test
for `cmd/buildctl-daemon`.

## Files

- `Dockerfile`: minimal build context (`FROM docker.io/library/alpine:3.20`).
- `smoke/Dockerfile`: context used by canary smoke builds.
- `integration.sh`: creates temporary contexts, registries, and BuildKit.

## Recommended test

Run the automated integration test described below. It creates and removes all
of its Docker resources. The manual API flow requires you to provide a running
BuildKit on `127.0.0.1:9094` and a Registry on `localhost:5000` first.

## Start the API daemon

From the repository root:

```bash
zip -j /tmp/buildctl-daemon-source.zip cmd/buildctl-daemon/test/Dockerfile

go run ./cmd/buildctl-daemon \
  --listen 127.0.0.1:18080 \
  --buildkitd-addr tcp://127.0.0.1:9094 \
  --work-dir /tmp/buildctl-daemon-api-test
```

If auth is enabled, add `--auth-token test-token` and include `-H 'Authorization: Bearer test-token'` in every `/v1/*` curl request below.

## Health check

```bash
curl -sS http://127.0.0.1:18080/healthz
```

## Create a nydus build

```bash
curl -sS -X POST \
  -F file=@/tmp/buildctl-daemon-source.zip \
  -F image=localhost:5000/node:latest \
  -F image_type=nydus \
  http://127.0.0.1:18080/v1/builds
```

The nydus output image will be:

```text
localhost:5000/node:latest_nydus_v3
```

## Query one build

Replace `<id>` with the id returned by create:

```bash
curl -sS http://127.0.0.1:18080/v1/builds/<id>
```

## Create and wait synchronously

`sync=true` waits until the task reaches a terminal state, then returns the same JSON shape as async status. Logs and task status remain available until `--keep-ttl` expires.

```bash
curl -sS -X POST \
  -F file=@/tmp/buildctl-daemon-source.zip \
  -F image=localhost:5000/node:latest \
  -F image_type=nydus \
  -F sync=true \
  http://127.0.0.1:18080/v1/builds
```

## Read build logs

```bash
curl -sS 'http://127.0.0.1:18080/v1/builds/<id>/logs?tail_bytes=20000'
```

## List builds

```bash
curl -sS http://127.0.0.1:18080/v1/builds
```

## Cancel a build

```bash
curl -sS -X DELETE http://127.0.0.1:18080/v1/builds/<id>
```

Equivalent endpoint:

```bash
curl -sS -X POST http://127.0.0.1:18080/v1/builds/<id>/cancel
```

## Verify pushed manifest

After the task status becomes `succeeded`:

```bash
curl -fsS http://localhost:5000/v2/node/manifests/latest_nydus_v3 \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
  -o /tmp/node-latest-nydus-v3-manifest.json

wc -c /tmp/node-latest-nydus-v3-manifest.json
```

## Automated integration test

`integration.sh` starts two temporary Docker Registry instances and one privileged BuildKit container, starts the daemon with `default_mode` and `production_mode`, and verifies:

- a legacy request without `mode` remains in `default_mode` and keeps its original target;
- a `production_mode` request receives a different `routed_target`;
- BuildKit actually pushes the production image to the routed registry and its manifest is readable;
- `image_type=both` runs OCI first, then uses the routed OCI manifest digest in the generated Nydus `FROM`.

Requirements: a running Docker daemon, `go`, `curl`, `jq`, and `zip`. The script uses `registry:2` and `moby/buildkit:v0.22.0`, then removes all temporary containers and its Docker network on exit. Vanilla `moby/buildkit:v0.22.0` does not include the Nydus exporter, so the default run verifies the OCI push and digest-pinned `FROM` orchestration and expects the exporter to reject `compression=nydus`.

```bash
./cmd/buildctl-daemon/test/integration.sh
```

To require both manifests to be pushed, run the same test with a Nydus-enabled BuildKit image:

```bash
BUILDCTL_DAEMON_INTEGRATION_BUILDKIT_IMAGE='<nydus-enabled-buildkit-image>' \
  ./cmd/buildctl-daemon/test/integration.sh
```

Queue isolation itself is covered by the Go HTTP integration test with a blocking fake BuildKit runner. This keeps the concurrency assertion deterministic, while the Docker test covers the real BuildKit and registry push path.
