# Agent Guidelines & Features Skill

Welcome! If you are an AI coding assistant or agent developing features or fixing issues in the **Smart Router**, you are required to follow the standard guidelines and practices of this repository.

## 🚀 Core Agent Skill File

All structured guidelines, repository design patterns, model parameters, and Test-Driven Development (TDD) tutorials are maintained in the dedicated skill file:

👉 **[SKILL.md](skills/agent_guidelines/SKILL.md)**

---

## ⚠️ Crucial Rules & Requirements

Before you write any code or create configuration schemas, keep these rules in mind:

### 1. Model Version Requirements
- **Baseline & Latest Series**: You MUST never use, route to, or configure any Gemini model version earlier than the **Gemini 2.5** series or Gemma earlier than **Gemma 4**. Prefer the latest authorized models:
  - **Gemini 4**: `gemini-4-argon`.
  - **Gemini 3.8**: `gemini-3.8-flash` (Note: `gemini-3.8-pro` and `gemini-3.8-flash-lite` do NOT exist and will fail validation).
  - **Gemini 3.5**: `gemini-3.5-flash`, `gemini-3.5-flash-lite`.
  - **Gemini 3.1**: `gemini-3.1-pro`, `gemini-3.1-flash-lite`.
  - **Gemini 2.5**: `gemini-2.5-flash`, `gemini-2.5-pro`, `gemini-2.5-flash-lite`.
  - **Gemma 4**: `gemma-4-26b`, `gemma-4-31b`, `gemma-4-4b`.
- **Deprecated Models**: Never route or configure legacy models like `gemini-1.5-flash`, `gemini-2.0-flash`, or `gemma2:2b`.

### 2. Test-Driven Development (TDD) & Never-Mock Policy
- Always write failing integration or unit tests in `backend/proxy/proxy_test.go` or `frontend/dashboard/dashboard_test.go` **first** before implementing the matching logic.
- **Integration Test Requirement**: Every new proxy feature MUST have at least one end-to-end integration test under `backend/proxy/proxy_test.go` through the full `RouterProxy.ServeHTTP` loop.
- **Strict Never-Mock Directive**: Never introduce synthetic metrics, mock cost generators, or simulation mode toggles in runtime handlers. All dashboard views must bind to real backend APIs and live telemetry, surfacing empty states or explicit error banners when upstream data is unavailable.
- Run local testing routinely across all packages:
  ```bash
  go test -v ./pkg/... ./backend/... ./frontend/...
  ```

### 3. Reusing Design Patterns & UI Architecture
- **App-Centric Architecture**: Ensure priority, RPM, and TPM rate limits are mapped to an **App** (inheriting fallback limits from the parent **Client** when unspecified).
- **Crisp Slate & Cobalt 2–3 Panel UI**: Keep the admin dashboard aligned with the 3-panel layout (Left core domain navigation, Center workspace with compact Intent Pills, and Right expandable Contextual Docs & Quick Reference drawer) with dynamic `PROXY_PUBLIC_URL` endpoint injection.
- **Local Development (`LOCAL_DEV`)**: Use `LOCAL_DEV=true` with local data `data/local_db.json` when writing local tests or spinning up the proxy locally.
- **Declarative Custom Headers**: Use `CustomHeader` configuration definitions instead of manually parsing and validating specific headers in Go code.

### 4. Documentation Synchronicity
- **Mandatory Update Rule**: You **MUST** update or append to the corresponding files under the **`/docs`** directory in the same pull request/commit whenever adding a new feature, refactoring routing schemas, or altering dashboard workflows.
- **Maintain Tone**: Ensure all updates adhere to the crisp, terse, active, and "humanized" technical writing style of the docset.

### 5. Monorepo Code Layout & Shared Package Strategy
- **Unified Codebase (`pkg/config`)**: To prevent schema drift and code duplication, shared configuration structures (`Client`, `App`, `APIKey`, `RoutingRule`, `CustomHeader`, `ModelConfig`) and validation helpers must be kept under `pkg/config` at the root. **Do not** duplicate these configurations inside individual backend or frontend directories.
- **CLI Utility Boundaries (`cmd/verify`)**: Post-deployment CLI verification scripts and standalone binaries must reside inside the root `cmd/` folder. Do not embed client tools inside runtime service directories.
- **Docker Scoping**: Both `backend/Dockerfile` and `frontend/Dockerfile` run from the repository root build context (`.`) so they can securely compile with `pkg/`.

### 6. IDE Zero-Warning Policy
- **Unconditional Resolution**: You **MUST** check for, address, and fix any compiler errors, warnings, or linter information/style suggestions (e.g., gopls warnings, scanner error checking, unused variables/imports, or switch tag suggestions) identified by the IDE/compiler in the codebase before marking any task as done.
- **Zero Warnings**: Never leave code warnings unresolved in files you touched.

For detailed code examples and tutorials on how to implement these patterns via TDD, please open and execute the instructions in **[skills/agent_guidelines/SKILL.md](skills/agent_guidelines/SKILL.md)**.


