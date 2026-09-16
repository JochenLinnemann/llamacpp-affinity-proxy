# Architectural Decisions

This file records decisions that shape the system over time.
The goal is clarity, not perfection.

### Decision: Standard-library reverse proxy with in-memory affinity
**Status:** Accepted  
**Context:** The service needs to stay small while supporting streaming OpenAI-compatible proxying and dynamic slot pinning for `llama.cpp`.  
**Decision:** Implement the proxy in Go using `net/http` and `httputil.ReverseProxy`, with a mutex-protected in-memory LRU affinity table keyed by normalized conversation IDs.  
**Consequences:** The service remains small, dependency-light, and easy to containerize. Affinity is lost on restart and does not coordinate across multiple proxy instances. Supported rewriteable request bodies are size-limited to keep memory usage bounded.  
**Date:** 2026-09-16

### Decision: Header-based conversation identity only for v1
**Status:** Accepted  
**Context:** Stable affinity requires a conversation identifier, but inferring one from arbitrary request bodies would add coupling to client payload formats and risk incorrect routing.  
**Decision:** Accept stable IDs only from `X-Conversation-Id`, `X-Hermes-Session-Id`, and `X-KiloCode-TaskId`, and normalize them into `X-Conversation-Id` for the backend.  
**Consequences:** Integration stays explicit and predictable. Clients must send one of the supported headers or the proxy will forward requests without dynamic `id_slot` injection. Diagnostics that expose affinity state are limited to loopback callers and return redacted identifiers, and operational logs use redacted conversation identifiers.  
**Date:** 2026-09-16
