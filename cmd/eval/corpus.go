package main

// EvalCase represents a single hard / adversarial prompt routing benchmark item.
type EvalCase struct {
	ID                    string `json:"id"`
	Category              string `json:"category"`
	TrapDescription       string `json:"trap_description"`
	Prompt                string `json:"prompt"`
	HasTools              bool   `json:"has_tools,omitempty"`
	HasMultimodal         bool   `json:"has_multimodal,omitempty"`
	ExpectedTier          string `json:"expected_tier"`           // "simple", "medium", "complex"
	ExpectedLocalEligible bool   `json:"expected_local_eligible"` // true if local open-weights (Gemma 4) can safely handle
}

// BuildAdversarialCorpus constructs 20 hard routing test patterns across 5 adversarial categories
// specifically designed to expose failure modes in char-length heuristics, keyword regex classifiers,
// and uncalibrated routers.
func BuildAdversarialCorpus() []EvalCase {
	return []EvalCase{
		// -------------------------------------------------------------------------
		// Category 1: deceptive_brevity (<= 90 chars, zero code fences or obvious trigger words,
		// yet requires formal mathematical, security, or distributed-systems reasoning -> complex)
		// -------------------------------------------------------------------------
		{
			ID:                    "db-01-2pc-crash",
			Category:              "deceptive_brevity",
			TrapDescription:       "66-char distributed consensus failure question with zero code/math regex keywords",
			Prompt:                "Find a blocking state in 2PC if coordinator dies right after prepare.",
			ExpectedTier:          "complex",
			ExpectedLocalEligible: false,
		},
		{
			ID:                    "db-02-greedy-counterexample",
			Category:              "deceptive_brevity",
			TrapDescription:       "65-char discrete math counterexample construction without 'proof' or O-notation",
			Prompt:                "Give a 4-coin system where greedy change-making fails for amount 6.",
			ExpectedTier:          "complex",
			ExpectedLocalEligible: false,
		},
		{
			ID:                    "db-03-ssrf-one-liner",
			Category:              "deceptive_brevity",
			TrapDescription:       "67-char cloud VPC SSRF security vulnerability audit disguised as a casual check",
			Prompt:                `Spot the risk: http.Get(r.URL.Query().Get("url")) inside a GCP VPC.`,
			ExpectedTier:          "complex",
			ExpectedLocalEligible: false,
		},
		{
			ID:                    "db-04-serializable-skew",
			Category:              "deceptive_brevity",
			TrapDescription:       "78-char database isolation anomaly reasoning under Snapshot vs Serializable",
			Prompt:                "How does Postgres SSI detect write skew without locking every scanned row?",
			ExpectedTier:          "complex",
			ExpectedLocalEligible: false,
		},

		// -------------------------------------------------------------------------
		// Category 2: verbose_boilerplate (>= 450 chars of noisy logs/JSON/boilerplate,
		// yet the actual user task is trivial single-field extraction or yes/no lookup -> simple)
		// -------------------------------------------------------------------------
		{
			ID:              "vb-01-shipping-zip",
			Category:        "verbose_boilerplate",
			TrapDescription: "620+ chars of verbose shipping disclaimer boilerplate around a trivial 5-digit ZIP extraction",
			Prompt: `Customer Fulfillment Notice — Standard Warehouse Dispatch Record (Ref #INV-2026-88412-US-WEST).
Please note that standard ground shipments are processed Monday through Friday excluding federal holidays, and carrier scan updates may take up to twenty-four hours to propagate across regional sorting hubs.
Recipient: Hannah Alvarez, Meadowbrook Botanical Supply Co., 1480 Terrazzo Way, Suite 310, Austin, TX 78704, United States.
Packaging instruction: Keep upright, ambient temperature between 55F and 78F, no signature waiver permitted at front desk.

Task: Output ONLY the 5-digit postal ZIP code from the address above.`,
			ExpectedTier:          "simple",
			ExpectedLocalEligible: true,
		},
		{
			ID:              "vb-02-startup-port-yesno",
			Category:        "verbose_boilerplate",
			TrapDescription: "580+ chars of mundane startup INFO log lines asking a binary yes/no port question",
			Prompt: `2026-11-11T18:00:01Z INFO bootstrap: loading static locale bundle en-US (42 entries)
2026-11-11T18:00:01Z INFO bootstrap: verified timezone database America/Denver
2026-11-11T18:00:02Z INFO assets: mounted static stylesheet manifest /static/app.css
2026-11-11T18:00:02Z INFO health: readiness probe registered on /healthz
2026-11-11T18:00:02Z INFO http: server listening cleanly on 0.0.0.0:8081 (TLS disabled for local dev)
2026-11-11T18:00:03Z INFO metrics: periodic flush interval set to 60s

Question: Is the HTTP server listening on port 8081? Answer only Yes or No.`,
			ExpectedTier:          "simple",
			ExpectedLocalEligible: true,
		},
		{
			ID:              "vb-03-meeting-sentiment",
			Category:        "verbose_boilerplate",
			TrapDescription: "560+ chars of office lunch catering chatter asking for 1-word sentiment classification",
			Prompt: `Hey everyone, just wanted to send a quick thank-you note after Friday's team lunch on the patio! The Mediterranean wraps and lemon herb potatoes arrived right on time at noon, nobody had any allergy mix-ups with the gluten-free trays, and we even had enough baklava left over for the afternoon coffee break. Everyone in the survey said they loved catching up in person and asked if we could order from the same cafe again next month.

Classify the overall sentiment of this message as one word: positive, neutral, or negative.`,
			ExpectedTier:          "simple",
			ExpectedLocalEligible: true,
		},
		{
			ID:              "vb-04-csv-row-count",
			Category:        "verbose_boilerplate",
			TrapDescription: "520+ chars of simple inventory CSV rows asking only for the SKU on the last line",
			Prompt: `sku,item_name,aisle,in_stock_units,reorder_threshold,supplier_region
SKU-1001,Ceramic Pour-Over Dripper White,Aisle-02,48,15,Pacific-Northwest
SKU-1002,Unbleached Paper Filters 100pk,Aisle-02,190,40,Midwest-Central
SKU-1003,Gooseneck Kettle Brushed Steel,Aisle-03,19,10,Northeast-Coast
SKU-1004,Insulated Glass Carafe 600ml,Aisle-03,34,12,Southwest-Hub
SKU-1005,Burr Grinder Cleaning Brush Set,Aisle-04,85,20,Mountain-West

Return only the SKU value from the very last row of the table above.`,
			ExpectedTier:          "simple",
			ExpectedLocalEligible: true,
		},

		// -------------------------------------------------------------------------
		// Category 3: homograph_camouflage (Everyday English words that collide with SQL/Go/Python
		// regex tokens like SELECT, FROM, WHERE, return, package, class, import, struct -> simple)
		// -------------------------------------------------------------------------
		{
			ID:                    "hc-01-retail-return-package",
			Category:              "homograph_camouflage",
			TrapDescription:       "Collides with regex tokens 'where', 'return', 'package', 'from', 'class', 'select'",
			Prompt:                "Where do I return a damaged package from Tuesday's pottery class if I select store credit?",
			ExpectedTier:          "simple",
			ExpectedLocalEligible: true,
		},
		{
			ID:                    "hc-02-customs-import-form",
			Category:              "homograph_camouflage",
			TrapDescription:       "Collides with regex tokens 'import', 'from', 'where', 'return' in travel FAQ triage",
			Prompt:                "Do travelers import olive oil duty-free from Italy, and where do we return the customs slip?",
			ExpectedTier:          "simple",
			ExpectedLocalEligible: true,
		},
		{
			ID:                    "hc-03-flight-class-upgrade",
			Category:              "homograph_camouflage",
			TrapDescription:       "Collides with 'select', 'class', 'from', 'return' in a simple airline greeting/FAQ",
			Prompt:                "Can I select Economy Plus class on my return flight from Denver? Reply in one short sentence.",
			ExpectedTier:          "simple",
			ExpectedLocalEligible: true,
		},
		{
			ID:                    "hc-04-library-insert-bookmark",
			Category:              "homograph_camouflage",
			TrapDescription:       "Collides with SQL tokens 'insert', 'where', 'from', 'return' in a library return hours lookup",
			Prompt:                "What time does the central library close on Sunday if I return a book at the drop slot?",
			ExpectedTier:          "simple",
			ExpectedLocalEligible: true,
		},

		// -------------------------------------------------------------------------
		// Category 4: jargon_in_trivial_task (Stuffed with CS/math buzzwords like O(n log n),
		// Byzantine consensus, Kubernetes, eigen, tradeoffs, yet task is trivial formatting/counting -> simple)
		// -------------------------------------------------------------------------
		{
			ID:                    "jt-01-capitalize-title",
			Category:              "jargon_in_trivial_task",
			TrapDescription:       "Contains 'why does', 'O(n log n)', 'matrix', 'algorithm', 'edge cases' inside a title-case task",
			Prompt:                `Convert this blog title to Title Case only (do not answer it): "why does our o(n log n) matrix algorithm hit edge cases?"`,
			ExpectedTier:          "simple",
			ExpectedLocalEligible: true,
		},
		{
			ID:                    "jt-02-word-count-buzzwords",
			Category:              "jargon_in_trivial_task",
			TrapDescription:       "Contains 'compare and contrast', 'async', 'await', 'struct', 'impl', 'tradeoffs' inside word-count",
			Prompt:                `How many words are inside the quotes? "Compare and contrast async await struct impl tradeoffs." Reply with just the integer.`,
			ExpectedTier:          "simple",
			ExpectedLocalEligible: true,
		},
		{
			ID:                    "jt-03-typo-fix-tweet",
			Category:              "jargon_in_trivial_task",
			TrapDescription:       "Contains 'architect', 'optimize', 'root cause', 'tensor' inside a 1-word spelling fix",
			Prompt:                `Fix the single misspelled word in this sentence: "Our systems architect helped optimize tensor throughput and find the root cuase."`,
			ExpectedTier:          "simple",
			ExpectedLocalEligible: true,
		},
		{
			ID:                    "jt-04-acronym-expansion",
			Category:              "jargon_in_trivial_task",
			TrapDescription:       "Mentions Raft consensus & Kubernetes in a simple acronym lookup",
			Prompt:                "What does the acronym CNCF stand for in Kubernetes open-source governance? One line only.",
			ExpectedTier:          "simple",
			ExpectedLocalEligible: true,
		},

		// -------------------------------------------------------------------------
		// Category 5: nuanced_medium (Moderate synthesis, translation, summarization, or small
		// utility conversion — too structured for ultra-lite triage, but does not need frontier pro -> medium)
		// -------------------------------------------------------------------------
		{
			ID:              "nm-01-changelog-summary",
			Category:        "nuanced_medium",
			TrapDescription: "3-bullet release note synthesis into polished customer prose with tone constraints",
			Prompt: `Rewrite these three internal engineering bullets into a single clear, customer-facing changelog paragraph (3-4 sentences, no internal ticket IDs):
- [SR-412] Added 30s heartbeat check before enqueueing onto local runner pools so offline nodes spill to cloud immediately.
- [SR-419] Preserved systemInstruction blocks when translating Gemini generateContent calls to local OpenAI-compatible runners.
- [SR-425] Added per-endpoint fallback model configuration in the admin modal.`,
			ExpectedTier:          "medium",
			ExpectedLocalEligible: true,
		},
		{
			ID:              "nm-02-japanese-doc-translation",
			Category:        "nuanced_medium",
			TrapDescription: "Technical documentation paragraph translation preserving backtick identifiers",
			Prompt:          "Translate the following developer note into natural, polite Japanese while keeping all header names in backticks unchanged: 'Set `X-Session-ID` on the initial request to pin multi-turn conversations to the selected model, or pass `X-Session-Policy: adaptive` to re-score each turn.'",
			ExpectedTier:          "medium",
			ExpectedLocalEligible: true,
		},
		{
			ID:              "nm-03-incident-status-note",
			Category:        "nuanced_medium",
			TrapDescription: "Structured operational status update with timeline, impact, and next update window",
			Prompt:          "Draft a concise status-page update for customers explaining that EU webhook deliveries experienced a 4-minute queue delay between 14:10 and 14:14 UTC due to a retry storm, no events were dropped, and backlog drain is now complete.",
			ExpectedTier:          "medium",
			ExpectedLocalEligible: true,
		},
		{
			ID:              "nm-04-jq-filter-explanation",
			Category:        "nuanced_medium",
			TrapDescription: "Practical CLI data-extraction snippet and brief explanation",
			Prompt:          "Write a `jq` command that reads an array of JSON objects from `events.json`, filters to items where `.status >= 500`, and outputs `.service` and `.latency_ms` as tab-separated lines, with a two-sentence explanation.",
			ExpectedTier:          "medium",
			ExpectedLocalEligible: true,
		},
	}
}
