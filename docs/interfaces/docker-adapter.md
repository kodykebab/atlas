# Docker adapter

The Docker adapter lets a Docker client manage Atlas VMs through a limited Docker Engine API. It supports detached VM creation and power operations with existing Atlas images. It does not run container images or guest commands.

The adapter forwards the caller's credentials to the [tenant API](tenant-api.md). Atlas owns VM records and permissions. The adapter stores no persistent state. See the [component specification](../../services/docker-adapter/SPEC.md) for code ownership and tests.

## Build and run

Use Go 1.26.6 or later. Atlas must accept `tags` on VM creation. Start the adapter on the same machine as the Docker client:

```sh
cd services/docker-adapter
go build -o /tmp/atlas-docker-adapter .
/tmp/atlas-docker-adapter -atlas-url https://atlas.example.com -listen 127.0.0.1:2375
```

The listener uses HTTP. Keep it on loopback. For a remote endpoint, use a TLS reverse proxy and a Docker client configured to verify its certificate. Forward authorization headers and disable response buffering for wait requests. Protect the adapter-to-Atlas connection with HTTPS. The client uses the system trust store; `SSL_CERT_FILE` can specify a private CA bundle.

The service runs as an ordinary user. It does not need Docker, root access, a database, or host storage. `make build` produces Linux amd64 and arm64 binaries under `dist/`.

## Configure the client

Obtain a tenant token using the existing [Atlas authentication flow](signing-keys.md). Create a separate Docker configuration directory. Replace the placeholders below with that token and its tenant ID:

```sh
mkdir -m 700 ~/.docker-atlas
(umask 077; cat > ~/.docker-atlas/config.json <<'JSON'
{"HttpHeaders":{"Authorization":"Bearer <Atlas token>","X-Tenant-ID":"<tenant ID>"}}
JSON
)
unset DOCKER_CONTEXT DOCKER_TLS DOCKER_TLS_VERIFY DOCKER_API_VERSION DOCKER_CUSTOM_HEADERS
export DOCKER_CONFIG=~/.docker-atlas
export DOCKER_HOST=tcp://127.0.0.1:2375
```

Use this configuration only with the Atlas adapter. Docker sends these headers to the selected host. The adapter does not refresh tokens. Replace an expired token in the file and retry the command after checking any pending VM operation. `docker login` supplies registry credentials, not Atlas credentials.

## Manage a VM

Use an enabled, available Atlas image ID or a unique exact title from `docker images`. Titles must be accepted by Docker's image-reference parser. Use the ID if a title is ambiguous or invalid. A `:latest` suffix on a title is ignored.

```sh
docker images
docker run -d --pull=never --name web --cpus 1 --memory 512m <image>
docker inspect web
docker ps -a
docker pause web
docker unpause web
docker stop web
docker wait web
docker start web
docker rm -f web
```

`--pull=never` prevents Docker from trying to pull an unknown image from a registry. Atlas images contain a guest operating system and boot services. The adapter supplies no SSH keys, attach stream, logs, or exec session. Use native Atlas access and guest configuration for administration.

## Supported commands

| Command | Behavior |
| --- | --- |
| `run -d --pull=never` | Create and boot a VM, then wait for running state. |
| `images` | List enabled, available Atlas images. |
| `ps`, `ps -a` | List managed VMs using Atlas's last reported state. |
| `inspect` | Read current VM state, size, hostname, and addresses. |
| `start`, `stop`, `pause`, `unpause` | Request power state and wait for matching desired and observed state. |
| `wait` | Wait for stopped state. Return synthetic code 0 for stopped or deleted, or code 1 with an error for failed. |
| `rm` | Remove a stopped or failed VM. Wait until Atlas returns 404. |
| `rm -f` | Remove a managed VM in any state that Atlas permits. |

Atlas starts a VM during creation. Ordinary `docker create` and foreground `docker run` request attachment, so the adapter rejects them before allocating a VM. Direct API clients that create without attachment must expect immediate boot.

Restart is unsupported because the public Atlas API does not expose restart completion. Use stop followed by start. Commands, environment variables, labels, TTY, stdin, mounts, ports, custom networks, GPU requests, restart policies, automatic removal, and additional resource limits are rejected.

Image pulls, build, push, attach, logs, exec, kill, networks, and volumes are unsupported. Nonempty filters, list limits, size queries, platform selection, and custom stop signals or timeouts are rejected. This is not a Docker Compose endpoint.

The adapter negotiates API 1.54 and accepts version prefixes 1.44 through 1.54 for this subset. Recorded request fixtures use Docker CLI 29.4.1. API version acceptance does not imply full Engine compatibility.

## Configuration

| Flag | Default | Meaning |
| --- | --- | --- |
| `-atlas-url` | Required | Atlas HTTP(S) origin, without credentials or a path. |
| `-listen` | `127.0.0.1:2375` | Docker API listener. |
| `-cpu-millicores` | `1000` | CPU allocation when `--cpus` is absent. |
| `-memory-mib` | `1024` | Memory allocation when `--memory` is absent. |
| `-disk-mib` | `10240` | Root disk allocation for each new VM. |
| `-poll-interval` | `1s` | Time between state observations. |
| `-operation-timeout` | `5m` | Budget for lookup, mutation, and completion checks. |

CPU allocation is 100 to 32000 millicores. `--cpus` accepts multiples of 0.001. Memory must be positive whole MiB. Choose a disk size that meets the selected image's requirement. Atlas validates capacity and image compatibility.

Backend calls have a 30-second timeout within the operation budget. Request bodies have a 30-second read deadline and a 1 MiB create-body limit. A wait lookup has the operation budget; its stream continues until completion, an upstream error, disconnection, or shutdown. Shutdown cancels active requests and allows five seconds for handlers to finish.

## State and recovery

Docker IDs are Atlas VM IDs. The adapter resolves names through the `docker.name` tag and selects only VMs with `docker.managed=true`. It stores the requested image reference in `docker.image`. Keep these tags intact while using Docker commands.

Names are not atomic allocations. Simultaneous creates can produce duplicate names. The adapter rejects ambiguous name lookups; use full VM IDs to inspect or remove duplicates. A full ID takes precedence over a matching name.

Mutations are not retried automatically. A timeout or lost response can occur after Atlas accepted a request. Check `docker ps -a` and `docker inspect` before retrying. If create returned no ID, use the requested name or native Atlas records to find the VM. A failed run can leave a VM that needs explicit removal. Native Atlas termination protection still applies to forced removal.

`ps` uses a stored observation and can lag `inspect`. Unknown and pending observations render as Docker `created`; they do not prove that the VM has never booted. Power completion checks are observations, not operation-generation acknowledgements. Another Atlas caller can change the desired state during a Docker operation.

Wait and inspect exit codes describe VM state only. They are not guest process exit codes. `next-exit` uses polling and can miss a complete run/stop transition. Guest process IDs and start/finish timestamps are unavailable. Do not use this adapter to judge guest workload success.

Each identity lookup scans the caller's managed VM pages. Each waiter polls separately. Fleet-scale cost is not measured. Disconnect abandoned waits and choose a polling interval suitable for the region.
