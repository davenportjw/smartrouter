package templates

// MetricsViewModel holds data to display in the Monitoring tab.
type MetricsViewModel struct {
	TotalRequests int
	PeakRate      int
	P95LatencyMs  int
	ErrorRate     float64

	VolumeChartSVG  string
	LatencyChartSVG string
}

// CostTransaction represents an individual session billing log.
type CostTransaction struct {
	Time          string  `json:"time"`
	SessionID     string  `json:"session_id"`
	ClientID      string  `json:"client_id"`
	ModelRouted   string  `json:"model_routed"`
	InputTokens   int     `json:"input_tokens"`
	OutputTokens  int     `json:"output_tokens"`
	EstimatedCost float64 `json:"estimated_cost"`
}

// ModelCostBreakdown represents spend analytics per model type.
type ModelCostBreakdown struct {
	ModelName string  `json:"model_name"`
	Cost      float64 `json:"cost"`
	Percent   float64 `json:"percent"`
}

// ClientCostBreakdown represents spend analytics per client.
type ClientCostBreakdown struct {
	ClientID string  `json:"client_id"`
	Cost     float64 `json:"cost"`
	Percent  float64 `json:"percent"`
}

// CostsViewModel holds data to display in the Cost Analytics tab.
type CostsViewModel struct {
	TotalSpend       float64
	TotalTokensInput int
	TotalTokensOutput int
	AvgCostPer1K     float64

	ModelBreakdowns  []ModelCostBreakdown
	ClientBreakdowns []ClientCostBreakdown
	RecentSessions   []CostTransaction

	ModelCostSVG  string
	ClientCostSVG string
}
