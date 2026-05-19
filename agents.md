# Agent Guidelines & Features Skill

Welcome! If you are an AI coding assistant or agent developing features or fixing issues in the **Gemini Smart Router**, you are required to follow the standard guidelines and practices of this repository.

## 🚀 Core Agent Skill File

All structured guidelines, repository design patterns, model parameters, and Test-Driven Development (TDD) tutorials are maintained in the dedicated skill file:

👉 **[agent_guidelines.md](skills/agent_guidelines.md)**

---

## ⚠️ Crucial Rules & Requirements

Before you write any code or create configuration schemas, keep these rules in mind:

### 1. Model Version Requirements
- **Baseline Requirement**: You MUST never use, route to, or configure any Gemini model version earlier than the **Gemini 2.5** series (e.g., `gemini-2.5-flash`, `gemini-2.5-pro`, `gemini-2.5-flash-lite`).
- **Deprecated Models**: Avoid routing or configuring legacy models like `gemini-1.5-flash` or `gemini-2.0-flash` for active production environments.

### 2. Test-Driven Development (TDD)
- Always write failing integration or unit tests in `pkg/proxy/proxy_test.go` **first** before implementing the matching logic inside the proxy or config packages.
- **Integration Test Requirement**: Every new feature MUST have at least one end-to-end integration test (or multiple if required) under `pkg/proxy/proxy_test.go`. Isolated unit testing of helper subcomponents is insufficient; features must be tested through the full `RouterProxy.ServeHTTP` loop.
- Run local testing routinely:
  ```bash
  go test -v ./pkg/...
  ```

### 3. Reusing Design Patterns
- **App-Centric Architecture**: Ensure priority, RPM, and TPM rate limits are mapped to an **App** instead of just the Client.
- **Local Mocking (`LOCAL_DEV`)**: Use `LOCAL_DEV=true` with local data `data/local_db.json` when writing local tests or spinning up the proxy locally.
- **Declarative Custom Headers**: Use `CustomHeader` configuration definitions instead of manually parsing and validating specific headers in Go code.

For detailed code examples and tutorials on how to implement these patterns via TDD, please open and execute the instructions in **[skills/agent_guidelines.md](skills/agent_guidelines.md)**.
