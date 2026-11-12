# Smart Router Engineering TODOs

## Decision Engine Benchmarking & Evaluation (`cmd/eval`)

Currently, `cmd/eval` enforces a readiness gate (`IsExternalProviderBenchmarkable` via `GET /api/v1/complexity/keys`) and benchmarks `gemini-decision` (`gemini-decision-1`), `llm` (`gemini-3.5-flash-lite` generative JSON), and static `heuristics`, automatically skipping external/cluster adapters whose credentials or remote endpoints are not provisioned.

- [ ] **Benchmark Live Jev / Typesafe (`engine: "jev"`)**:
  - Provision `JEV_API_KEY` in Google Cloud Secret Manager (`printf "%s" "<your-key>" | gcloud secrets versions add JEV_API_KEY --data-file=- --project=<your-gcp-project-id>`) and configure `JEV_ENDPOINT` on `gemini-smart-router`.
  - Verify `GET /api/v1/complexity/keys` reports `configured: true` and `ExecutionMode: "remote_api"` (never `local_fallback`), then run `cmd/eval` to compare tier accuracy, Brier calibration, and `p50`/`p95` latency against `gemini-decision-1`.
- [ ] **Benchmark Live Fastino / Pioneer (`engine: "fastino"`)**:
  - Provision `FASTINO_API_KEY` in Google Cloud Secret Manager and configure `FASTINO_ENDPOINT` on `gemini-smart-router`.
  - Run the 20-prompt adversarial suite (`cmd/eval`) once `IsExternalProviderBenchmarkable` unlocks `fastino`, recording calibrated simplex Brier scores and cost per 1,000 routes.
- [ ] **Benchmark Live Contrastive-LM Runner (`engine: "contrastive-lm"`)**:
  - Deploy a live `contrastive-lm` sidecar/runner endpoint (local workstation or GKE Spot pool) and wire `CLM_ENDPOINT` (plus `CLM_API_KEY` in Secret Manager if authenticated).
  - Benchmark live contrastive embedding scores (`ExecutionMode: "remote_api"`) against `deceptive_brevity` and `homograph_camouflage` traps in `cmd/eval`.
- [ ] **Benchmark Alternative Generative & Local Open-Weights Classifiers (`engine: "llm"`)**:
  - Extend `cmd/eval` `-llm-models` flag to benchmark `gemini-3.8-flash`, `gemma-4-4b`, and `gemma-4-26b` (via active local runner or direct vLLM/`llama.cpp` endpoint) as `ClassifierModel` baselines against `gemini-decision-1`.
- [ ] **Expand Adversarial Corpus & Optional CI Threshold Gate**:
  - Expand `BuildAdversarialCorpus()` (`cmd/eval/corpus.go`) from 20 to 50+ prompts covering multi-turn diffs, multilingual domain jargon, and large log attachments.
  - Add `-min-score` and `-require-providers` CLI flags to `cmd/eval` for automated release gating.
