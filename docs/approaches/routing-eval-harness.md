# 🎯 Adversarial Routing Evaluation Harness (`cmd/eval`)

`cmd/eval` benchmarks and grades live Smart Router decision engines (`gemini-decision`, `llm`, `heuristics`, plus any configured remote/cluster providers `jev`, `fastino`, and `contrastive-lm`) against adversarial prompt patterns and live hybrid local-vs-cloud failover invariants.

> [!IMPORTANT]
> **Fair Provider Readiness Gate (`IsExternalProviderBenchmarkable`)**
> Before benchmarking external providers (`jev`, `fastino`, `contrastive-lm`), `cmd/eval` queries `GET /api/v1/complexity/keys`. If an external provider's remote endpoint or API key (`JEV_API_KEY`, `FASTINO_API_KEY`, `CLM_ENDPOINT`) is unconfigured, `cmd/eval` automatically omits it from the head-to-head scorecard and records it under **Skipped Unconfigured External Providers** so built-in offline fallback stubs are never misrepresented as third-party service results.

---

## 🚀 Running the Harness

Run against a live Cloud Run deployment (authenticates via ADC service-account impersonation, seeds ephemeral Firestore fixtures, and tears down all test records automatically on exit or `SIGINT`):

```bash
go run ./cmd/eval \
  -url https://gemini-smart-router-<hash>-uc.a.run.app \
  -project <your-gcp-project-id> \
  -out-json /tmp/eval-report.json \
  -out-md /tmp/eval-report.md
```

Run unit tests for the corpus invariants and scoring rubric:

```bash
go test -v ./cmd/eval/...
```

---

## 🧪 Adversarial Prompt Categories (20 Cases × Configured Engines)

Standard length or keyword benchmarks overestimate router quality. `BuildAdversarialCorpus()` in `cmd/eval/corpus.go` tests five failure-mode traps:

1. **`deceptive_brevity` (`<= 90` chars, `complex`)**: Compact distributed-systems, concurrency, discrete math, and SSRF security questions written without code blocks or regex trigger words (`proof`, `O(n log n)`, `func`). Exposes character-limit under-routing.
2. **`verbose_boilerplate` (`>= 450` chars, `simple`)**: Multi-paragraph shipping records, startup `INFO` logs, CSV rows, and team chatter paired with trivial single-token extraction or binary `Yes/No` lookups. Exposes length-based over-provisioning waste.
3. **`homograph_camouflage` (`simple`)**: Everyday retail, travel, and library questions containing English words (`where`, `select`, `from`, `return`, `package`, `class`, `import`) that collide with SQL/Go/Python code-detection regexes.
4. **`jargon_in_trivial_task` (`simple`)**: Trivial title-capitalization, word-count, and typo-fix instructions wrapping technical buzzwords (`O(n log n)`, `async await struct impl`, `tensor`, `Kubernetes`).
5. **`nuanced_medium` (`medium`)**: Multi-bullet release-note synthesis, technical translation preserving backtick identifiers, operational status updates, and `jq` CLI snippets that exceed ultra-lite triage without requiring a frontier reasoning tier.

---

## ⚡ Live End-to-End Proxy Stress Scenarios (7 Scenarios)

Beyond `/api/v1/complexity/inspect`, `cmd/eval` executes seven live HTTP scenarios through `RouterProxy.ServeHTTP`:

1. **Deceptive Brevity E2E Route**: Verifies a 66-character 2PC crash prompt routes to `gemini-4-argon` via `X-Router-Engine: gemini-decision`.
2. **Verbose Boilerplate Cost Guard**: Verifies a 620+ character address record routes to `gemini-3.5-flash-lite` instead of burning frontier tokens.
3. **16-Char Tool-Call Forced Escalation**: Verifies `ForceComplexTools` escalates tool-bearing requests to `gemini-4-argon` regardless of prompt brevity.
4. **Multimodal Inline Image Escalation**: Verifies `ForceComplexMultimodal` escalates inline image payloads cleanly.
5. **Sticky Session Lock + Mid-Session Task Switch**: Verifies Turn 1 locks `gemini-3.5-flash-lite` under `X-Session-ID`, Turn 2 switches the lock to `gemini-4-argon` on `X-Agent-Task: security_audit`, and Turn 3 preserves the upgraded lock.
6. **Classifier-Conditional Local Gemma 4 + `<1ms` Cloud Spillover**: Verifies a Stage-4 rule (`decision_engine: "gemini-decision"`, `complexity_tier: "simple"`, `min_confidence: 0.65`) targets a local `gemma-4-26b` pool and spills immediately (`X-Cluster-Fallback-Reason: no_online_runners`) to `gemini-3.8-flash` when zero runners are online.
7. **Native `/v1beta/decisions` Zero-Output-Token Contract**: Verifies `POST /v1beta/decisions` returns calibrated simplex probabilities summing to `1.0` with `total_output_tokens == 0`.

---

## 📊 100-Point Router Rating Rubric (`ScoreEngine`)

Each engine receives a composite score out of `100` (`A+` through `F`):

| Dimension | Max Points | Formula |
| :--- | :---: | :--- |
| **Adversarial Tier Accuracy** | `35` | `35 * (correct / total)` across all 20 adversarial cases |
| **Under-Routing Safety** | `25` | `25 * clamp(1 - 2.0*criticalUnderRoute - 0.75*minorUnderRoute - 0.25*overProvision, 0, 1)` |
| **Probability Calibration & Local Signal** | `15` | Multi-class Brier skill (`10 pts`) + `local_eligible` discrimination accuracy (`5 pts`); uncalibrated engines cap at `9 pts` |
| **Live Proxy Policy & Failover Invariants** | `15` | `15 * (passedScenarios / 7)` |
| **Routing Overhead Tail Latency** | `10` | Full `10 pts` at `p95 <= 350 ms`, decaying linearly to `0 pts` at `p95 >= 2000 ms` |

---

## 📝 Follow-Up TODOs: External & Cluster Decision Models

See [TODO.md](../../TODO.md) at the repository root for the checklist to provision and benchmark the remaining decision adapters once their live endpoints and Secret Manager credentials are configured:

- **Jev / Typesafe (`jev`)**: Provision `JEV_API_KEY` in Secret Manager + set `JEV_ENDPOINT` so `ExecutionMode: "remote_api"` is active, then re-run `cmd/eval`.
- **Fastino / Pioneer (`fastino`)**: Provision `FASTINO_API_KEY` in Secret Manager + set `FASTINO_ENDPOINT`, then benchmark live latency and Brier calibration in `cmd/eval`.
- **Contrastive-LM (`contrastive-lm`)**: Stand up a live `CLM_ENDPOINT` runner (local or GKE Spot pool) and benchmark real contrastive anchor embeddings instead of the offline 5D fallback.
- **Local Gemma 4 Generative Classifiers (`llm`)**: Benchmark `gemma-4-4b` and `gemma-4-26b` as `ClassifierModel` targets alongside `gemini-3.5-flash-lite` and `gemini-decision-1`.
