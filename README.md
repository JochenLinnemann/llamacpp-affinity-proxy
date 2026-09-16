# llamacpp-affinity-proxy

A lightweight conversation-affinity proxy for `llama.cpp`.

`llamacpp-affinity-proxy` keeps long-running LLM conversations attached to the same `llama.cpp` inference slot for as long as possible.

This improves KV-cache reuse when multiple clients or agent sessions share a `llama-server` running with `--parallel N`.

## Why?

`llama.cpp` can keep the KV state of a conversation in an inference slot. Reusing that slot allows subsequent turns with the same prompt prefix to avoid reprocessing large parts of the conversation.

With multiple concurrent clients, however, requests are normally scheduled onto available slots without persistent conversation affinity.

For long-running conversations this can lead to unnecessary cache churn:

```text
Conversation A → Slot 0
Conversation B → Slot 1
Conversation C → Slot 2
Conversation D → Slot 3

later...

Conversation A → Slot 2
```

If Slot 2 contains a different KV state, much of Conversation A may have to be prefetched again.

This becomes particularly expensive with large context windows such as 32K, 64K, or 128K tokens.

`llamacpp-affinity-proxy` adds the missing affinity layer:

```text
Conversation A ──────────────→ Slot 0
Conversation B ──────────────→ Slot 1
Conversation C ──────────────→ Slot 2
Conversation D ──────────────→ Slot 3
                                  │
                         llama.cpp server
```

As long as a conversation remains mapped to a slot, subsequent requests are routed back to that slot.

## How it works

The proxy sits transparently between OpenAI-compatible clients and `llama.cpp`:

```text
OpenWebUI ─┐
Hermes ────┼──→ llamacpp-affinity-proxy ───→ llama.cpp
Kilo Code ─┘          :8001                    :8801
```

Clients continue to use a normal OpenAI-compatible API.

For generation requests, the proxy:

1. determines a stable conversation identifier from request headers;
2. normalizes the identifier;
3. assigns the conversation to one of the available `llama.cpp` slots;
4. reuses that slot for subsequent requests;
5. injects the corresponding `id_slot` into the request body;
6. transparently proxies the request and response.

Streaming responses remain streaming responses.

## Conversation identifiers

The proxy is intended to understand conversation identifiers from several clients.

### OpenWebUI

OpenWebUI can supply its chat ID as:

```text
X-Conversation-Id: owui:<chat-id>
```

For example, configure the connection header using OpenWebUI's chat-ID substitution:

```text
X-Conversation-Id: owui:{{CHAT_ID}}
```

A raw `X-Conversation-Id` can also be normalized by the proxy.

### Hermes Agent

Hermes sessions can be identified by:

```text
X-Hermes-Session-Id: <session-id>
```

The proxy normalizes this to:

```text
X-Conversation-Id: hermes:<session-id>
```

### Kilo Code

Kilo tasks can be identified by:

```text
X-KiloCode-TaskId: <task-id>
```

The proxy normalizes this to:

```text
X-Conversation-Id: kilo:<task-id>
```

The client must actually provide a stable identifier. The proxy deliberately does not attempt to infer conversation identity from prompt contents.

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

Further requests from `hermes:abc` continue to use Slot 0.

When all slots are occupied and a new conversation arrives, the least recently used mapping can be evicted:

```text
before:

Slot 0 → hermes:abc
Slot 1 → owui:def
Slot 2 → kilo:ghi
Slot 3 → owui:jkl

new conversation:
owui:xyz

after LRU eviction:

Slot 0 → owui:xyz
Slot 1 → owui:def
Slot 2 → kilo:ghi
Slot 3 → owui:jkl
```

The proxy manages **slot affinity**, not the KV cache itself. `llama.cpp` remains responsible for inference and KV-cache storage.

## Transparent deployment

A useful deployment pattern is to let the proxy take over the existing `llamacpp` service name:

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

This allows existing applications to keep their current host and port configuration.

Example Docker Compose layout:

```yaml
services:
  llamacpp:
    # llamacpp-affinity-proxy
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

    # GPU/device configuration omitted here.

    command:
      - --host
      - "0.0.0.0"

      - --port
      - "8801"

      - --parallel
      - "4"

      # model and remaining llama.cpp options...
```

The backend port does not need to be published to the host when both services share the same Docker network.

## Configuration

The planned configuration is intentionally small:

| Variable | Default | Description |
| --- | --- | --- |
| `LISTEN_ADDR` | `:8001` | Address on which the proxy listens |
| `BACKEND_URL` | `http://llcpp-backend:8801` | llama.cpp backend |
| `SLOT_COUNT` | `4` | Number of llama.cpp inference slots |

`SLOT_COUNT` should match the effective number of slots configured through `llama.cpp --parallel`.

## Diagnostics

The proxy exposes a small diagnostics endpoint:

```text
GET /_affinity
```

Example:

```json
{
  "slot_count": 4,
  "slots": [
    {
      "slot": 0,
      "conversation_id": "hermes:abc",
      "last_used": "2026-09-16T14:00:00Z"
    },
    {
      "slot": 1,
      "conversation_id": "owui:def",
      "last_used": "2026-09-16T14:01:00Z"
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

A simple process health endpoint is available at:

```text
GET /health
```

## Scope

The project intentionally has a narrow responsibility:

> Maintain conversation-to-slot affinity in front of a parallel `llama.cpp` server.

It does **not** intend to become another LLM runtime, API gateway, or agent framework.

In particular, the initial implementation does not provide:

- KV-cache serialization;
- RAM or SSD KV-cache offloading;
- persistent affinity mappings across restarts;
- prompt hashing or prompt-based conversation detection;
- authentication;
- model management;
- request queues;
- distributed inference.

These may be considered separately where they make sense, but they are not required for the core affinity problem.

## Why not save and restore KV caches?

`llama.cpp` provides mechanisms for saving and restoring slot state, but support and behavior vary with model architecture and inference features.

The first goal of this project is therefore deliberately simpler:

**avoid unnecessary slot changes before trying to restore a slot after it has already been lost.**

KV persistence may become an optional extension later, but it is not required for conversation affinity.

## Design principles

The proxy should remain:

- **transparent** — existing OpenAI-compatible clients should continue to work;
- **small** — preferably a small Go service with minimal dependencies;
- **streaming-safe** — SSE responses must not be buffered;
- **model-agnostic** — no assumptions about a particular model or quantization;
- **runtime-focused** — prompts and messages are not interpreted or modified;
- **observable** — affinity decisions should be visible without logging prompt contents;
- **boring** — no infrastructure is added unless the problem requires it.

## Project status

This project is currently in early development.

The initial milestone is:

- OpenAI-compatible transparent reverse proxy;
- dynamic conversation → slot affinity;
- LRU slot assignment;
- support for OpenWebUI, Hermes Agent, and Kilo Code identifiers;
- streaming passthrough;
- health and affinity diagnostics;
- Docker deployment.

See [ROADMAP.md](ROADMAP.md) for planned work and [ARCHITECTURE.md](ARCHITECTURE.md) for architectural notes.

## License

MIT
