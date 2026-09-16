# llamacpp-affinity-proxy

`llamacpp-affinity-proxy` is a small Go reverse proxy that sits in front of `llama.cpp` and assigns a stable `id_slot` to each conversation.

Clients keep talking to:

`http://llamacpp:8001`

The proxy forwards requests to:

`http://llcpp-backend:8801`

## What it does

- normalizes conversation headers into `X-Conversation-Id`
- keeps an in-memory `conversation -> slot` affinity table
- reuses existing slots for returning conversations
- assigns free slots to new conversations
- evicts the least recently used conversation when all slots are busy
- injects `id_slot` into JSON bodies for supported generation endpoints
- transparently proxies streaming responses without buffering the full response

Supported generation endpoints:

- `POST /v1/chat/completions`
- `POST /v1/completions`
- `POST /v1/responses`

Additional endpoints:

- `GET /health`
- `GET /_affinity`

Other routes are transparently proxied.

## Conversation header normalization

Headers are checked in this priority order:

1. `X-Conversation-Id`
2. `X-Hermes-Session-Id`
3. `X-KiloCode-TaskId`

Normalization rules:

- `X-Conversation-Id: <id>` -> `owui:<id>` unless already namespaced
- `X-Hermes-Session-Id: <id>` -> `hermes:<id>`
- `X-KiloCode-TaskId: <id>` -> `kilo:<id>`
- already namespaced values such as `owui:...`, `hermes:...`, and `kilo:...` are forwarded unchanged

If no supported conversation header is present, the proxy does not inject `id_slot`.

## Slot affinity behavior

Configure the number of backend slots with `SLOT_COUNT`.

- existing conversations always reuse their slot
- new conversations take a free slot when available
- when all slots are busy, the least recently used conversation is evicted
- affinity state is in memory only and is lost on restart
- KV-cache save/restore is intentionally out of scope for v1

## Configuration

Environment variables:

- `LISTEN_ADDR` default `:8001`
- `BACKEND_URL` default `http://llcpp-backend:8801`
- `SLOT_COUNT` default `4`

The proxy validates configuration on startup and exits with a clear error for invalid values.

## Docker Compose example

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
      BACKEND_URL: http://llcpp-backend:8801
      SLOT_COUNT: "4"
    depends_on:
      - llcpp-backend

  llcpp-backend:
    image: ghcr.io/ggml-org/llama.cpp:server-vulkan
    container_name: llcpp-backend
    restart: unless-stopped
    command:
      - --host
      - "0.0.0.0"
      - --port
      - "8801"
      - --parallel
      - "4"
```

Do not publish port `8801` to the host unless you explicitly need it for debugging.

## Client examples

### OpenWebUI

Send:

`X-Conversation-Id: owui:{{CHAT_ID}}`

If OpenWebUI sends the raw chat ID instead, the proxy prefixes it with `owui:`.

### Hermes

Send:

`X-Hermes-Session-Id: <stable-session-id>`

The proxy forwards:

`X-Conversation-Id: hermes:<stable-session-id>`

### Kilo

Send:

`X-KiloCode-TaskId: <stable-task-id>`

The proxy forwards:

`X-Conversation-Id: kilo:<stable-task-id>`

Clients must send one of these headers. The proxy does not infer stable conversation IDs from the request body.

## Diagnostics

- `GET /health` returns `200 OK` when the proxy process is healthy
- `GET /_affinity` returns the current in-memory slot mapping and last-used timestamps

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
      "conversation_id": null
    }
  ]
}
```

## Architecture summary

- standard-library HTTP server using `net/http`
- standard-library reverse proxy using `httputil.ReverseProxy`
- concurrency-safe in-memory slot table guarded by a mutex
- request-body rewriting only for supported JSON generation endpoints
- transparent streaming from backend to client

## Limitations

Version 1 intentionally does not implement:

- KV save/restore
- persistent affinity storage
- Redis or database state
- authentication
- Prometheus metrics
- automatic conversation detection from prompts or messages
- retries on different slots

## Local development

Run tests:

```bash
go test ./...
go vet ./...
```

Build:

```bash
go build ./...
```

Run locally:

```bash
LISTEN_ADDR=:8001 BACKEND_URL=http://localhost:8801 SLOT_COUNT=4 go run .
```
