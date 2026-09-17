# llamacpp-affinity-proxy

A lightweight conversation-affinity proxy for `llama.cpp`.

`llamacpp-affinity-proxy` keeps long-running LLM conversations attached to the same `llama.cpp` inference slot for as long as possible.

This helps improve KV-cache reuse when multiple clients or agent sessions share a `llama-server` running with `--parallel N`.

## Project status

This project is in early development. The initial Go implementation is under review in [PR #1](https://github.com/JochenLinnemann/llamacpp-affinity-proxy/pull/1).

The behavior described below represents the intended initial feature set. Build and development instructions apply to a checkout containing the implementation.

## Why?

`llama.cpp` can retain a conversation's KV state in an inference slot. Reusing that slot with the same prompt prefix can avoid processing large parts of the conversation again.

When multiple clients share a backend, a conversation may return to a different slot:

```text
Conversation A → Slot 0
Conversation B → Slot 1
Conversation C → Slot 2
Conversation D → Slot 3

later...

Conversation A → Slot 2
```

If Slot 2 contains a different KV state, much of Conversation A's prompt may need to be processed again.

This becomes particularly expensive with large context windows such as 32K, 64K, or 128K tokens.

`llamacpp-affinity-proxy` adds explicit conversation affinity:

```text
Conversation A ──────────────→ Slot 0
Conversation B ──────────────→ Slot 1
Conversation C ──────────────→ Slot 2
Conversation D ──────────────→ Slot 3
                                  │
                         llama.cpp server
```

As long as a conversation remains mapped to a slot, subsequent requests are routed back to that slot.

The proxy manages **slot affinity**, not the KV cache itself. `llama.cpp` remains responsible for inference, prompt caching, and KV-cache storage.

## How it works

The proxy sits between OpenAI-compatible clients and `llama.cpp`:

```text
OpenWebUI ─┐
Hermes ────┼──→ llamacpp-affinity-proxy ───→ llama.cpp
Kilo Code ─┘          :8001                    :8801
```

For supported JSON generation requests, the proxy:

1. reads a stable conversation identifier from request headers;
2. normalizes the identifier;
3. assigns or reuses a backend slot;
4. injects the corresponding `id_slot` into the request body;
5. forwards the request and streams the backend response.

Supported generation endpoints:

- `POST /v1/chat/completions`
- `POST /v1/completions`
- `POST /v1/responses`

Slot injection applies to requests with `Content-Type: application/json`, including parameters such as `charset=utf-8`.

Requests without a supported conversation header are forwarded without dynamic `id_slot` injection. Other endpoints, including model listing, tokenization, and unknown routes, are proxied without slot assignment.

The proxy does not interpret prompts, messages, tools, reasoning, multimodal data, or model settings. Request rewriting is limited to the top-level `id_slot` field.

## Conversation identifiers

Headers are checked in this priority order. The first non-empty value is used:

1. `X-Conversation-Id`
2. `X-Hermes-Session-Id`
3. `X-KiloCode-TaskId`
4. `X-Session-Id`
5. `X-Session-Affinity`

Normalization rules:

| Incoming header | Normalized identifier |
| --- | --- |
| `X-Conversation-Id: <id>` | `owui:<id>` |
| `X-Hermes-Session-Id: <id>` | `hermes:<id>` |
| `X-KiloCode-TaskId: <id>` | `kilo:<id>` |
| `X-Session-Id: <id>` | `kilo:<id>` |
| `X-Session-Affinity: <id>` | `kilo:<id>` |

Already namespaced values using `owui:`, `hermes:`, or `kilo:` are preserved without adding another prefix.

The normalized identifier is forwarded as `X-Conversation-Id`. The Hermes and all Kilo source headers are removed when an identifier is selected. Conversation IDs are written unredacted to affinity log messages and to the loopback-only `/_affinity` diagnostics endpoint; protect access to the proxy and its logs accordingly.

The client must actually provide a stable identifier. The proxy does not infer conversation identity from prompt contents.

### OpenWebUI

Send the chat ID as:

```text
X-Conversation-Id: owui:<chat-id>
```

If your OpenWebUI connection supports chat-ID substitution, configure:

```text
X-Conversation-Id: owui:{{CHAT_ID}}
```

A raw chat ID is also accepted and prefixed with `owui:`.

### Hermes Agent

Send:

```text
X-Hermes-Session-Id: <stable-session-id>
```

The proxy forwards:

```text
X-Conversation-Id: hermes:<stable-session-id>
```

### Kilo Code

Send:

```text
X-KiloCode-TaskId: <stable-task-id>
```

The proxy forwards:

```text
X-Conversation-Id: kilo:<stable-task-id>
```

## Dynamic slot assignment

Assume `llama.cpp` runs with:

```text
--parallel 4
```

and the proxy is configured with:

```text
SLOT_COUNT=4
```

The first conversations are assigned free slots:

```text
hermes:abc  → Slot 0
owui:def    → Slot 1
kilo:ghi    → Slot 2
owui:jkl    → Slot 3
```

Further requests from `hermes:abc` reuse Slot 0 while that mapping exists.

When every slot has a mapping and a new conversation arrives, the intended policy is to evict the least recently used conversation:

```text
before:

Slot 0 → hermes:abc
Slot 1 → owui:def
Slot 2 → kilo:ghi
Slot 3 → owui:jkl

new conversation:
owui:xyz

after eviction, assuming hermes:abc was least recently used:

Slot 0 → owui:xyz
Slot 1 → owui:def
Slot 2 → kilo:ghi
Slot 3 → owui:jkl
```

Affinity state is stored in memory and is lost when the proxy restarts. Eviction removes the mapping; it does not save the evicted conversation's KV state.

### Explicit `id_slot`

For requests participating in conversation affinity:

- an existing conversation may specify its currently assigned slot;
- a new conversation may explicitly select a free slot;
- a conflicting or out-of-range slot returns HTTP 400;
- `id_slot: null` is treated as unspecified and replaced with an assigned slot.

Requests without a conversation identifier are passed through, including any client-provided `id_slot`.

Rewritten JSON request bodies are limited to 32 MiB. Oversized bodies are rejected before forwarding.

## Transparent deployment

The proxy can take over the existing `llamacpp` service name:

```text
before:

client → llamacpp:8001

after:

client → llamacpp:8001
             │
             │ affinity proxy
             ▼
        llcpp-backend:8801
             │
             │ llama.cpp
             ▼
            GPU
```

Existing applications can keep their host and port configuration. To enable affinity, they add one of the supported conversation headers.

Example Docker Compose layout:

```yaml
services:
  llamacpp:
    build:
      context: ./llamacpp-affinity-proxy
    container_name: llamacpp
    restart: unless-stopped
    ports:
      - "8001:8001"
    environment:
      LISTEN_ADDR: ":8001"
      BACKEND_URL: "http://llcpp-backend:8801"
      SLOT_COUNT: "4"
    depends_on:
      - llcpp-backend

  llcpp-backend:
    image: ghcr.io/ggml-org/llama.cpp:server-vulkan
    container_name: llcpp-backend
    restart: unless-stopped

    # Add GPU/device configuration for your environment.

    command:
      - --host
      - "0.0.0.0"
      - --port
      - "8801"
      - --parallel
      - "4"

      # Add your model and remaining llama.cpp options.
```

This is a deployment layout, not a complete model configuration. Add the required model options, mounts, and GPU/device settings.

The backend port does not need to be published to the host when both services share the same Docker network.

The proxy uses a multi-stage Docker build and a minimal, non-root runtime image. It requires no writable filesystem state.

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `LISTEN_ADDR` | `:8001` | Address on which the proxy listens |
| `BACKEND_URL` | `http://llcpp-backend:8801` | llama.cpp backend URL |
| `BACKEND_RESPONSE_HEADER_TIMEOUT` | `5m0s` | Maximum time to wait for backend response headers before failing the request |
| `SLOT_COUNT` | `4` | Number of backend inference slots |

`SLOT_COUNT` should match the effective number of slots configured through `llama.cpp --parallel`.

`BACKEND_URL` must include an HTTP or HTTPS scheme and a host. `SLOT_COUNT` must be a positive integer.

Backend availability is not required for the proxy process to start.

## Streaming and errors

Streaming responses are forwarded incrementally using Go's standard-library `httputil.ReverseProxy`.

The proxy preserves backend status codes, response bodies, content types, and applicable response headers. Client cancellation propagates to the backend request.

The public listener applies a bounded request-header read timeout, and backend response-header waits are limited by `BACKEND_RESPONSE_HEADER_TIMEOUT` without limiting streaming after headers arrive.

Connection failures and backend response-header timeouts return HTTP 502. Invalid JSON or invalid/conflicting explicit slots in requests requiring rewriting return HTTP 400.

## Diagnostics

### Process health

```text
GET /health
```

Returns `200 OK` when the proxy process is serving requests. This is a process health check, not a backend readiness check.

### Affinity state

```text
GET /_affinity
```

Returns the current slot mappings and last-used timestamps.

Access is restricted to loopback callers using the direct TCP peer address. Forwarded headers do not grant access. In Docker, callers must connect from within the proxy container's network namespace.

Conversation identifiers are redacted:

```json
{
  "slot_count": 4,
  "slots": [
    {
      "slot": 0,
      "conversation_id": "hermes:56a3eef54dda",
      "last_used": "2026-09-16T14:00:00Z"
    },
    {
      "slot": 1,
      "conversation_id": null
    },
    {
      "slot": 2,
      "conversation_id": null
    },
    {
      "slot": 3,
      "conversation_id": null
    }
  ]
}
```

Operational logs record assignments, reuse, evictions, and backend errors using redacted conversation identifiers. Prompts, message bodies, API keys, and authorization headers are not intentionally logged.

## Scope and limitations

The project has a narrow responsibility:

> Maintain conversation-to-slot affinity in front of a parallel llama.cpp server.

The initial implementation does not provide:

- KV-cache serialization or save/restore;
- RAM or SSD KV-cache offloading;
- persistent affinity mappings across restarts;
- coordination across multiple proxy instances;
- prompt hashing or prompt-based conversation detection;
- authentication or rate limiting;
- model management;
- proxy-managed request queues;
- automatic retries on different slots;
- Prometheus metrics;
- distributed inference.

It is designed for one proxy process in front of one backend. Requests that bypass affinity can still use backend slots and affect their cached contents.

## Why not save and restore KV caches?

`llama.cpp` provides mechanisms for saving and restoring slot state, but compatibility and behavior depend on the backend configuration and inference features.

The first goal is to reduce unnecessary slot changes. KV persistence may become an optional extension later.

## Design principles

The proxy should remain:

- **transparent** — existing OpenAI-compatible clients should continue to work;
- **small** — a Go service using the standard library;
- **streaming-safe** — SSE responses must not be buffered in full;
- **model-agnostic** — no assumptions about a particular model or quantization;
- **runtime-focused** — prompts and messages are not interpreted;
- **observable** — affinity decisions are visible without logging prompt contents;
- **simple to operate** — no persistent infrastructure is required.

## Local development

Use Go 1.24 or later.

Run tests and static checks:

```bash
go test ./...
go test -race ./...
go vet ./...
```

Build:

```bash
go build ./...
```

Run against a local backend:

```bash
LISTEN_ADDR=:8001 \
BACKEND_URL=http://localhost:8801 \
SLOT_COUNT=4 \
go run .
```

Example request:

```bash
curl http://localhost:8001/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'X-Hermes-Session-Id: example-session' \
  -d '{
    "model": "local-model",
    "messages": [
      {"role": "user", "content": "Hello!"}
    ],
    "stream": true
  }' \
  --no-buffer
```

See [ROADMAP.md](ROADMAP.md) for planned work, [ARCHITECTURE.md](ARCHITECTURE.md) for architectural notes, and [DECISIONS.md](DECISIONS.md) for design decisions.

## License

MIT
