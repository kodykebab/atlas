# Docker adapter specification

## Purpose

This independent Go module translates a subset of the Docker Engine API into the Atlas tenant API. See the [operator guide](../../docs/interfaces/docker-adapter.md) for configuration, command behavior, and recovery.

```text
Docker client -> engine.Server -> atlas.Client -> Atlas tenant API -> Metal
```

## Code ownership

| File | Contract |
| --- | --- |
| `main.go` | Validate flags, own the HTTP listener, cancel requests, and await shutdown. |
| `internal/atlas/atlas.go` | Forward caller credentials, read all list pages, validate responses, and preserve upstream errors. |
| `internal/engine/server.go` | Negotiate API versions, dispatch routes, admit query options, and encode errors. |
| `internal/engine/admission.go` | Validate the create body and convert resource units before Atlas access. |
| `internal/engine/containers.go` | Resolve managed identities and translate VM lifecycle operations. |
| `internal/engine/wait.go` | Poll state, acknowledge wait streams, and report completion or failure. |
| `internal/engine/images.go` | List and resolve available Atlas images. |

Atlas owns VM records, authorization, tags, and desired state. Metal owns observed runtime state. The adapter holds no persistent records or credentials. Each request owns its polling and cancellation.

## Invariants

- Each backend request carries that caller's `Authorization` and `X-Tenant-ID`. Redirects and automatic mutation retries are disabled.
- Container resolution uses only tenant-visible VMs marked `docker.managed=true`. A full ID wins over a name. Ambiguous names and prefixes fail.
- Create admission rejects unsupported options before any backend request. Only known empty Docker defaults are ignored.
- Lifecycle completion requires matching desired and current power states. Removal requires an authenticated Atlas 404 after an accepted delete.
- Wait sends headers before polling so detached Docker run can send start. Lookup has an operation deadline. The stream ends on completion, upstream error, cancellation, or shutdown.
- A malformed or oversized upstream response fails instead of becoming an empty object or a successful mutation.

## Extension points

Add a Docker option in admission and its owner together. Test the actual Docker request and the Atlas mapping. Do not accept an option without corresponding Atlas behavior.

New lifecycle behavior must use the public Atlas API. Exact exit events, restart completion, and atomic Docker names require an Atlas contract before they can be guaranteed here.

## Validation

Run `make vet`, `make test`, and `make build` from this directory. Tests use loopback HTTP servers. Install a Docker CLI to run `TestDockerCLI`; it uses an isolated configuration and a fake Atlas, without a Docker daemon. CI requires the CLI and prints its version.

The fixture in `testdata/cli-29.4.1-requests.jsonl` records Docker 29.4.1 defaults. Tests cover admission, identity, pagination, credentials, redirects, malformed replies, partial failures, request deadlines, wait headers, CLI lifecycle, and graceful shutdown.
