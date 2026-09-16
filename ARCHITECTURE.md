# Architecture Overview

## System Goals
- Keep the proxy small, explicit, and easy to operate.
- Preserve OpenAI-compatible request and streaming behavior.
- Provide deterministic conversation-to-slot routing for `llama.cpp`.
- Avoid persistent infrastructure for v1.

## Non-Goals
- Persistent affinity or KV-cache state.
- Authentication, authorization, or rate limiting.
- Automatic conversation detection from prompts or payload contents.
- Metrics, dashboards, or external coordination systems.

## High-Level View

```text
client -> llamacpp-affinity-proxy (:8001) -> llama.cpp backend (:8801)
```

Request flow:
1. Read conversation headers in priority order.
2. Normalize to `X-Conversation-Id`.
3. For supported JSON generation endpoints, assign or reuse a slot.
4. Inject `id_slot` into the JSON body.
5. Stream the backend response back to the client unchanged.

## Major Components

### HTTP proxy server
- **Responsibility:** expose `/health`, `/_affinity`, and proxy all other routes.
- **Inputs / Outputs:** HTTP requests and responses.
- **Data ownership:** none.

### Affinity manager
- **Responsibility:** maintain in-memory `conversation -> slot` and `slot -> conversation` mappings.
- **Inputs / Outputs:** normalized conversation IDs, assigned slot IDs, diagnostic snapshots.
- **Data ownership:** slot occupancy and last-used timestamps.

### Request rewriter
- **Responsibility:** add `id_slot` to supported JSON generation requests.
- **Inputs / Outputs:** request body in, rewritten request body out.
- **Data ownership:** none; does not inspect prompts or model settings beyond `id_slot`.

## External Dependencies
- `llama.cpp` OpenAI-compatible HTTP server at `BACKEND_URL`
- Go standard library only

## Scaling Assumptions
- Designed for a single proxy process in front of one `llama.cpp` service.
- Slot count is small and configured explicitly with `SLOT_COUNT`.
- In-memory mutex-based coordination is sufficient for this scope.

## Constraints
- No writable filesystem state required.
- Must support streaming responses without buffering the whole response.
- Must not log prompts, API keys, authorization headers, or message bodies.
- `/_affinity` is intended for loopback callers only.
- Rewriteable JSON request bodies are capped to bound in-process memory usage.
