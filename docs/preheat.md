# Dragonfly Image Preheating

Preheats images into all Dragonfly seed peer caches via the Scheduler gRPC API
`scheduler.v2.Scheduler.PreheatImage`, accelerating subsequent image pulls.

## Usage

[preheat.sh](../scripts/preheat.sh) triggers preheating with a single command;
provide an image list:

```bash
./scripts/preheat.sh --image-list ./images.txt
```

Prerequisites:

- `kubectl` access that can create Jobs in the target namespace
- Dragonfly Scheduler v2 reachable from that namespace
- Pod egress to every registry in the image list
- Access to the published multi-architecture preheat helper image, or a custom
	image supplied with `--image`

One image reference per line (multi-level repo paths and `@sha256:` digests
supported; `#` lines and blank lines are ignored). The list is limited to
64 KiB so it remains safely below the container environment size limit:

```text
registry.example.com/library/ubuntu:22.04
registry.example.com/myns/myapp:v1.0.0
registry.example.com/myns/myapp@sha256:abcd...   # digests work too
```

The script creates a **uniquely-named Job** using the repository's preheat
helper image (with `grpcurl` and `jq` bundled), resolves each image reference to
`https://HOST/v2/REPO/manifests/REF`, and calls the Scheduler to preheat, one
image every 10s by default.

The script **returns immediately** after submission — preheating completes on
the cluster side. Kubernetes reclaims the Job and Pod `--ttl` seconds (default
600s) after completion. Multiple users can run concurrently without
interference (Job names carry a random suffix).

For a private registry, store credentials in a Secret in the same namespace.
The Secret must contain `username` and `password` keys:

```bash
secret_dir="$(mktemp -d)"
trap 'rm -rf "$secret_dir"' EXIT
umask 077
printf '%s' '<registry-user>' > "$secret_dir/username"
read -rsp 'Registry password: ' REGISTRY_PASSWORD; echo
printf '%s' "$REGISTRY_PASSWORD" > "$secret_dir/password"
unset REGISTRY_PASSWORD

kubectl -n default create secret generic registry-auth \
	--from-file=username="$secret_dir/username" \
	--from-file=password="$secret_dir/password"

./scripts/preheat.sh \
	--image-list ./images.txt \
	--registry-secret registry-auth \
	--scheduler-tls \
	--scheduler-ca-secret dragonfly-scheduler-ca
```

The Job references the Secret; it does not place credentials in command-line
arguments or inline Job values. Users who can read Secrets or exec into the Job
Pod may still access them, so use namespace RBAC and delete the Secret when it
is no longer needed. One invocation applies one credential pair to every image
in its list; use separate Jobs for different registries/accounts.

Track progress:

```bash
kubectl -n default logs -f job/<job-name>   # <job-name> is printed by the script
kubectl -n default get job <job-name>
```

## Options

| Option | Default | Description |
| --- | --- | --- |
| `--image-list FILE` | required | Image list file, one reference per line (`#`/blank ignored) |
| `--registry-secret NAME` | empty | Secret containing `username` and `password` keys. |
| `--namespace NS` | `default` | Target namespace |
| `--name NAME` | `preheat-<random>` | Job name; do not fix it in multi-user scenarios |
| `--scheduler-addr ADDR` | `dragonfly-scheduler.dragonfly-system.svc.cluster.local:8002` | Scheduler gRPC address |
| `--scheduler-tls` | off | Use TLS for the Scheduler gRPC connection; required with Registry credentials. |
| `--scheduler-ca-secret NAME` | empty | Secret containing `ca.crt` for Scheduler TLS. |
| `--scheduler-server-name NAME` | Scheduler host | TLS certificate server name override. |
| `--interval SECONDS` | `10` | Seconds between preheats |
| `--ttl SECONDS` | `600` | Seconds before the finished Job is reclaimed |
| `--image IMAGE` | `ghcr.io/inclusionai/buildkit-service/preheat:latest` | Helper image containing `bash`, `grpcurl`, `jq`, and CA certificates. |
| `--kubeconfig FILE` | `$KUBECONFIG` | kubeconfig path |
| `--insecure-skip-verify` | off | Disable registry TLS verification; use only for a trusted test registry. |

Registry TLS certificates are verified by default. Scheduler gRPC uses
plaintext unless `--scheduler-tls` is set. Private Registry credentials are
rejected in plaintext mode; use Scheduler TLS and, for a private CA, provide a
Secret containing `ca.crt` with `--scheduler-ca-secret`.

Run `./scripts/preheat.sh --help` for full help.
