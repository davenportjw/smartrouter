---
name: compatibility_audit
description: Verification guidelines and automated testing procedures to ensure enterprise API surface compatibility for Gemini Smart Router.
---

# Skill: Gemini Smart Router Enterprise Compatibility Audit

This skill enables the agent to execute a scheduled or manual audit of the Gemini Smart Router to ensure it remains fully compatible with the Gemini Enterprise Agent Platform API surface.

## Prerequisites
- Ensure that you are in the workspace root (the directory containing `main.go`).
- The Go compiler and tools must be available.

## Execution Guide

### Phase 1: Codebase Surface Verification
To verify that the smart router has route definitions for models, reasoningEngines, and ragCorpora, read `main.go` and confirm the presence of the following handlers:
- `/v1/models/`, `/v1beta/models/`
- `/v1/reasoningEngines/`, `/v1beta/reasoningEngines/`, `/v1beta1/reasoningEngines/`
- `/v1/ragCorpora/`, `/v1beta/ragCorpora/`, `/v1beta1/ragCorpora/`

### Phase 2: Run the Automated Integration Test
Execute the package proxy test suite to verify path rewriting and credential translation logic. Run the following shell command:

```bash
go test -v ./pkg/proxy/...
```

**Verification Criteria**:
- `TestRouterProxyCompatibility` must pass.
- The output must verify:
  - Standard model requests (`/v1/models/...`) translate to `/v1/projects/.../locations/.../publishers/google/models/...`.
  - Reasoning engine list/get/query requests (`/v1/reasoningEngines/...`) translate to `/v1/projects/.../locations/.../reasoningEngines/...`.
  - RAG corpora requests (`/v1/ragCorpora/...`) translate to `/v1/projects/.../locations/.../ragCorpora/...`.
  - Client-side API keys (`key` query param and `x-goog-api-key` header) are scrubbed.
  - Upstream authorization header `Authorization: Bearer ...` is injected.

### Phase 3: Live Environment Verification (Optional)
If live Vertex AI credentials are authenticated, verify a real endpoint resolution using curl against a local dev instance:
1. Start the router locally:
   ```bash
   LOCAL_DEV=true PORT=8080 go run main.go
   ```
2. Send a test query through the local smart router targeting standard models:
   ```bash
   curl -X POST "http://localhost:8080/v1/models/gemini-1.5-flash:generateContent?key=gr_dev_key_123456" \
     -H "Content-Type: application/json" \
     -d '{"contents":[{"parts":[{"text":"Hello"}]}]}'
   ```
3. Verify that you receive a successful JSON response and that the smart router console prints a structured access log containing `model_requested`, `model_routed`, and a `200` status code.
