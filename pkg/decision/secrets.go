package decision

import (
	"bufio"
	"os"
	"strings"
	"sync"

	"geminirouter/pkg/config"
)

// EngineSecretStatus describes the masked configuration state of an engine's API key, endpoint, and cluster connection mode.
// Raw secret values are NEVER exposed through this struct.
type EngineSecretStatus struct {
	Engine         string `json:"engine"`
	DisplayName    string `json:"display_name"`
	SecretName     string `json:"secret_name"`
	EnvVar         string `json:"env_var"`
	Configured     bool   `json:"configured"`
	MaskedKey      string `json:"masked_key"`
	MaskedPreview  string `json:"masked_preview"`
	Source         string `json:"source"` // "secret_manager", "dotenv", "env", "memory", or "none"
	Endpoint       string `json:"endpoint,omitempty"`
	ConnectionMode string `json:"connection_mode"` // "local", "cluster", or "remote"
	ClusterID      string `json:"cluster_id,omitempty"`
}

type engineClusterEntry struct {
	ConnectionMode string
	ClusterID      string
	Endpoint       string
}

var (
	secretVaultMu        sync.RWMutex
	secretVault          = map[string]string{}
	dotenvLoadedKeys     = map[string]string{}
	engineClusterConfigs = map[string]engineClusterEntry{}
)

func init() {
	if os.Getenv("K_SERVICE") == "" {
		_ = LoadDotEnv()
	}
}

// LoadDotEnv loads environment variables from the given .env file paths (or default relative
// paths ".env", "../.env", "../../.env" when none are specified). Existing non-empty environment
// variables are never overwritten. Keys loaded from or matching .env are tracked so their
// source can be reported as "dotenv" in local development.
func LoadDotEnv(paths ...string) error {
	candidates := paths
	if len(candidates) == 0 {
		candidates = []string{".env", "../.env", "../../.env"}
	}

	var firstErr error
	for _, p := range candidates {
		f, err := os.Open(p)
		if err != nil {
			if len(paths) > 0 && firstErr == nil {
				firstErr = err
			}
			continue
		}

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			key, val, ok := parseDotEnvLine(scanner.Text())
			if !ok || key == "" {
				continue
			}
			existing := os.Getenv(key)
			if existing == "" {
				if val != "" {
					_ = os.Setenv(key, val)
					secretVaultMu.Lock()
					dotenvLoadedKeys[key] = val
					secretVaultMu.Unlock()
				}
			} else if existing == val && val != "" {
				secretVaultMu.Lock()
				dotenvLoadedKeys[key] = val
				secretVaultMu.Unlock()
			}
		}
		_ = f.Close()
		if err := scanner.Err(); err != nil && firstErr == nil {
			firstErr = err
		}
		if len(paths) == 0 {
			// Stop after the first discovered default .env file
			break
		}
	}
	return firstErr
}

func parseDotEnvLine(line string) (string, string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	if strings.HasPrefix(trimmed, "export ") {
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "export "))
	}
	eqIdx := strings.IndexByte(trimmed, '=')
	if eqIdx <= 0 {
		return "", "", false
	}
	key := strings.TrimSpace(trimmed[:eqIdx])
	rawVal := strings.TrimSpace(trimmed[eqIdx+1:])
	if key == "" {
		return "", "", false
	}

	if len(rawVal) >= 2 && ((rawVal[0] == '"' && rawVal[len(rawVal)-1] == '"') || (rawVal[0] == '\'' && rawVal[len(rawVal)-1] == '\'')) {
		return key, rawVal[1 : len(rawVal)-1], true
	}

	// Strip inline comment outside quotes
	if hashIdx := strings.Index(rawVal, " #"); hashIdx != -1 {
		rawVal = strings.TrimSpace(rawVal[:hashIdx])
	} else if strings.HasPrefix(rawVal, "#") {
		rawVal = ""
	}
	rawVal = strings.Trim(rawVal, `"'`)
	return key, rawVal, true
}

func isDotEnvLoaded(key, val string) bool {
	if val == "" {
		return false
	}
	secretVaultMu.RLock()
	defer secretVaultMu.RUnlock()
	loadedVal, ok := dotenvLoadedKeys[key]
	return ok && loadedVal == val
}

// NormalizeEngineName canonicalizes engine names for secret lookup and rule matching.
func NormalizeEngineName(engine string) string {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "gemini-decision", "gemini-decisions", "gemini-decision-1", "decisions":
		return "gemini-decision"
	case "clm", "contrastive-lm", "contrastive_lm":
		return "contrastive-lm"
	case "fastino":
		return "fastino"
	case "jev":
		return "jev"
	case "llm":
		return "llm"
	case "heuristics", "heuristic":
		return "heuristics"
	default:
		return strings.ToLower(strings.TrimSpace(engine))
	}
}

// EngineMatches returns true if two engine identifiers refer to the same decision engine
// (handling "clm" and "contrastive-lm" interchangeably).
func EngineMatches(ruleEngine, activeEngine string) bool {
	if ruleEngine == "" || ruleEngine == "all" || ruleEngine == "*" {
		return true
	}
	return NormalizeEngineName(ruleEngine) == NormalizeEngineName(activeEngine)
}

// DefaultSecretRefForEngine returns the canonical Secret Manager / env var name for an engine.
func DefaultSecretRefForEngine(engine string) string {
	switch NormalizeEngineName(engine) {
	case "gemini-decision":
		return "GEMINI_DECISIONS_API_KEY"
	case "jev":
		return "JEV_API_KEY"
	case "fastino":
		return "FASTINO_API_KEY"
	case "contrastive-lm":
		return "CLM_API_KEY"
	default:
		return strings.ToUpper(NormalizeEngineName(engine)) + "_API_KEY"
	}
}

func candidateEnvVarsForEngine(engine, secretRef string) []string {
	var keys []string
	if trimmed := strings.TrimSpace(secretRef); trimmed != "" {
		keys = append(keys, trimmed)
	}
	switch NormalizeEngineName(engine) {
	case "gemini-decision":
		keys = append(keys, "GEMINI_DECISIONS_API_KEY")
	case "jev":
		keys = append(keys, "JEV_API_KEY", "TYPESAFE_API_KEY")
	case "fastino":
		keys = append(keys, "FASTINO_API_KEY", "PIONEER_API_KEY")
	case "contrastive-lm":
		keys = append(keys, "CLM_API_KEY", "CONTRASTIVE_LM_API_KEY")
	default:
		def := DefaultSecretRefForEngine(engine)
		if def != "" {
			keys = append(keys, def)
		}
	}
	return keys
}

// SetEngineSecret stores an API key in the in-memory secret vault under both the
// normalized engine name and its canonical secret ref. It is never persisted to disk.
func SetEngineSecret(engineOrRef, rawKey string) {
	trimmed := strings.TrimSpace(rawKey)
	if trimmed == "" {
		return
	}
	secretVaultMu.Lock()
	defer secretVaultMu.Unlock()

	norm := NormalizeEngineName(engineOrRef)
	secretVault[norm] = trimmed
	secretVault[DefaultSecretRefForEngine(norm)] = trimmed
	secretVault[strings.TrimSpace(engineOrRef)] = trimmed
}

// GetEngineSecret retrieves an API key from the in-memory secret vault.
func GetEngineSecret(engineOrRef string) (string, bool) {
	secretVaultMu.RLock()
	defer secretVaultMu.RUnlock()

	if v, ok := secretVault[strings.TrimSpace(engineOrRef)]; ok && v != "" {
		return v, true
	}
	norm := NormalizeEngineName(engineOrRef)
	if v, ok := secretVault[norm]; ok && v != "" {
		return v, true
	}
	if v, ok := secretVault[DefaultSecretRefForEngine(norm)]; ok && v != "" {
		return v, true
	}
	return "", false
}

// DeleteEngineSecret removes an API key from the in-memory secret vault.
func DeleteEngineSecret(engineOrRef string) {
	secretVaultMu.Lock()
	defer secretVaultMu.Unlock()

	norm := NormalizeEngineName(engineOrRef)
	delete(secretVault, strings.TrimSpace(engineOrRef))
	delete(secretVault, norm)
	delete(secretVault, DefaultSecretRefForEngine(norm))
}

// ResolveEngineSecret resolves the active bearer token for a decision engine:
// 1. Custom SecretRef or engine key in in-memory vault (if explicitly set at runtime/test)
// 2. Candidate environment variables (SecretRef, JEV_API_KEY/TYPESAFE_API_KEY, FASTINO_API_KEY/PIONEER_API_KEY, CLM_API_KEY/CONTRASTIVE_LM_API_KEY)
// 3. Reloads local .env in local mode if not yet populated
func ResolveEngineSecret(engine, secretRef string) string {
	if secretRef != "" {
		if v, ok := GetEngineSecret(secretRef); ok {
			return v
		}
	}
	if v, ok := GetEngineSecret(engine); ok {
		return v
	}

	candidates := candidateEnvVarsForEngine(engine, secretRef)
	for _, k := range candidates {
		if envVal := strings.TrimSpace(os.Getenv(k)); envVal != "" {
			return envVal
		}
	}

	if os.Getenv("K_SERVICE") == "" {
		_ = LoadDotEnv()
		for _, k := range candidates {
			if envVal := strings.TrimSpace(os.Getenv(k)); envVal != "" {
				return envVal
			}
		}
	}
	return ""
}

// MaskSecret returns a safe masked representation of a secret (e.g. "sk-...6789")
// so that raw keys are never exposed in UI templates or API responses.
func MaskSecret(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) <= 7 {
		return "..."
	}
	return trimmed[:3] + "..." + trimmed[len(trimmed)-4:]
}

func resolveEnvSecretWithSource(engine, secretRef string) (string, string, bool) {
	candidates := candidateEnvVarsForEngine(engine, secretRef)
	for _, k := range candidates {
		if envVal := strings.TrimSpace(os.Getenv(k)); envVal != "" {
			return k, envVal, true
		}
	}
	if os.Getenv("K_SERVICE") == "" {
		_ = LoadDotEnv()
		for _, k := range candidates {
			if envVal := strings.TrimSpace(os.Getenv(k)); envVal != "" {
				return k, envVal, true
			}
		}
	}
	return "", "", false
}

// DisplayNameForEngine returns the human-friendly provider label for a decision engine.
func DisplayNameForEngine(engine string) string {
	switch NormalizeEngineName(engine) {
	case "gemini-decision":
		return "Gemini Decisions API (gemini-decision-1 · Keyless ADC)"
	case "jev":
		return "Jev (Typesafe SystemOne)"
	case "fastino":
		return "Fastino (Pioneer GLiClass)"
	case "contrastive-lm":
		return "Contrastive-LM (5D Anchor Cluster / Local)"
	default:
		return engine
	}
}

// SetEngineEndpoint sets or clears the global provider endpoint URL for a decision engine.
func SetEngineEndpoint(engine, endpoint string) {
	norm := NormalizeEngineName(engine)
	if norm == "" {
		return
	}
	trimmed := strings.TrimSpace(endpoint)
	secretVaultMu.Lock()
	defer secretVaultMu.Unlock()

	cur := engineClusterConfigs[norm]
	cur.Endpoint = trimmed
	if trimmed == "" {
		if cur.ConnectionMode == "remote" || cur.ConnectionMode == "cluster" {
			cur.ConnectionMode = "local"
		}
	} else if cur.ConnectionMode == "" || cur.ConnectionMode == "local" {
		if cur.ClusterID != "" {
			cur.ConnectionMode = "cluster"
		} else {
			cur.ConnectionMode = "remote"
		}
	}
	engineClusterConfigs[norm] = cur
}

// GetEngineEndpoint returns the globally configured endpoint URL for a decision engine (or empty string if none).
func GetEngineEndpoint(engine string) string {
	norm := NormalizeEngineName(engine)
	secretVaultMu.RLock()
	cur, ok := engineClusterConfigs[norm]
	secretVaultMu.RUnlock()
	if ok {
		if cur.ConnectionMode == "local" && cur.Endpoint == "" {
			return ""
		}
		if cur.Endpoint != "" {
			return cur.Endpoint
		}
	}
	return ""
}

// SetEngineClusterConfig updates the endpoint, connection mode ("local", "cluster", "remote"), and cluster ID for a decision engine.
func SetEngineClusterConfig(engine, endpoint, connectionMode, clusterID string) {
	norm := NormalizeEngineName(engine)
	if norm == "" {
		return
	}
	mode := strings.ToLower(strings.TrimSpace(connectionMode))
	cid := strings.TrimSpace(clusterID)
	ep := strings.TrimSpace(endpoint)

	if mode == "local" {
		ep = ""
	} else if mode == "" {
		if ep != "" {
			if cid != "" {
				mode = "cluster"
			} else {
				mode = "remote"
			}
		} else {
			mode = "local"
		}
	}

	secretVaultMu.Lock()
	defer secretVaultMu.Unlock()
	engineClusterConfigs[norm] = engineClusterEntry{
		ConnectionMode: mode,
		ClusterID:      cid,
		Endpoint:       ep,
	}
}

// GetEngineClusterConfig returns the active endpoint, connection mode, and cluster ID for a decision engine.
func GetEngineClusterConfig(engine string) (endpoint, connectionMode, clusterID string) {
	norm := NormalizeEngineName(engine)
	secretVaultMu.RLock()
	cur, ok := engineClusterConfigs[norm]
	secretVaultMu.RUnlock()

	if ok && (cur.ConnectionMode != "" || cur.Endpoint != "" || cur.ClusterID != "") {
		mode := cur.ConnectionMode
		ep := cur.Endpoint
		if mode == "local" && ep == "" {
			return "", "local", cur.ClusterID
		}
		if ep != "" {
			if mode == "" {
				if cur.ClusterID != "" {
					mode = "cluster"
				} else {
					mode = "remote"
				}
			}
			return ep, mode, cur.ClusterID
		}
	}

	// Fallback to environment variable if configured
	var envEp string
	switch norm {
	case "gemini-decision":
		envEp = strings.TrimSpace(os.Getenv("GEMINI_DECISION_ENDPOINT"))
	case "jev":
		envEp = strings.TrimSpace(os.Getenv("JEV_ENDPOINT"))
	case "fastino":
		envEp = strings.TrimSpace(os.Getenv("FASTINO_ENDPOINT"))
	case "contrastive-lm":
		envEp = strings.TrimSpace(os.Getenv("CLM_ENDPOINT"))
		if envEp == "" {
			envEp = strings.TrimSpace(os.Getenv("CONTRASTIVE_LM_ENDPOINT"))
		}
	}
	if envEp != "" {
		mode := "remote"
		if norm == "contrastive-lm" {
			mode = "cluster"
		}
		return envEp, mode, cur.ClusterID
	}

	if norm == "gemini-decision" {
		return DefaultGeminiDecisionEndpoint, "adc", cur.ClusterID
	}

	return "", "local", cur.ClusterID
}

// GetEngineSecretStatus returns the masked status for a single engine.
// Source is reported as:
// - "secret_manager" when running on Cloud Run (K_SERVICE != "") and the env var is populated
// - "dotenv" when loaded from or matching the local .env file
// - "env" when set via process environment variable
// - "memory" when set via SetEngineSecret
// - "none" when unconfigured
func GetEngineSecretStatus(engine, secretRef string) EngineSecretStatus {
	norm := NormalizeEngineName(engine)
	envVar := DefaultSecretRefForEngine(norm)
	ref := strings.TrimSpace(secretRef)
	if ref == "" {
		ref = envVar
	}
	displayName := DisplayNameForEngine(norm)
	ep, mode, clusterID := GetEngineClusterConfig(norm)
	resolvedEndpoint := ep
	if resolvedEndpoint == "" {
		resolvedEndpoint = "local-embedded (in-process)"
	}

	if v, ok := GetEngineSecret(ref); ok {
		masked := MaskSecret(v)
		return EngineSecretStatus{
			Engine:         norm,
			DisplayName:    displayName,
			SecretName:     ref,
			EnvVar:         envVar,
			Configured:     true,
			MaskedKey:      masked,
			MaskedPreview:  masked,
			Source:         "memory",
			Endpoint:       resolvedEndpoint,
			ConnectionMode: mode,
			ClusterID:      clusterID,
		}
	}
	if v, ok := GetEngineSecret(norm); ok {
		masked := MaskSecret(v)
		return EngineSecretStatus{
			Engine:         norm,
			DisplayName:    displayName,
			SecretName:     ref,
			EnvVar:         envVar,
			Configured:     true,
			MaskedKey:      masked,
			MaskedPreview:  masked,
			Source:         "memory",
			Endpoint:       resolvedEndpoint,
			ConnectionMode: mode,
			ClusterID:      clusterID,
		}
	}

	if matchedKey, envVal, ok := resolveEnvSecretWithSource(norm, secretRef); ok {
		masked := MaskSecret(envVal)
		source := "env"
		if os.Getenv("K_SERVICE") != "" {
			source = "secret_manager"
		} else if isDotEnvLoaded(matchedKey, envVal) {
			source = "dotenv"
		}
		return EngineSecretStatus{
			Engine:         norm,
			DisplayName:    displayName,
			SecretName:     ref,
			EnvVar:         matchedKey,
			Configured:     true,
			MaskedKey:      masked,
			MaskedPreview:  masked,
			Source:         source,
			Endpoint:       resolvedEndpoint,
			ConnectionMode: mode,
			ClusterID:      clusterID,
		}
	}

	if norm == "gemini-decision" {
		if ep == "" {
			resolvedEndpoint = DefaultGeminiDecisionEndpoint
		}
		if mode == "" || mode == "local" {
			mode = "adc"
		}
		return EngineSecretStatus{
			Engine:         norm,
			DisplayName:    displayName,
			SecretName:     "ADC (OAuth2)",
			EnvVar:         "GOOGLE_CLOUD_PROJECT",
			Configured:     true,
			MaskedKey:      "ADC OAuth2",
			MaskedPreview:  "ADC OAuth2 (Keyless)",
			Source:         "adc",
			Endpoint:       resolvedEndpoint,
			ConnectionMode: mode,
			ClusterID:      clusterID,
		}
	}

	return EngineSecretStatus{
		Engine:         norm,
		DisplayName:    displayName,
		SecretName:     ref,
		EnvVar:         envVar,
		Configured:     false,
		MaskedKey:      "",
		MaskedPreview:  "",
		Source:         "none",
		Endpoint:       resolvedEndpoint,
		ConnectionMode: mode,
		ClusterID:      clusterID,
	}
}

// GetAllEngineSecretStatuses returns masked secret statuses for gemini-decision, jev, fastino, and contrastive-lm.
func GetAllEngineSecretStatuses(customSecretRef string) []EngineSecretStatus {
	return []EngineSecretStatus{
		GetEngineSecretStatus("gemini-decision", customSecretRef),
		GetEngineSecretStatus("jev", customSecretRef),
		GetEngineSecretStatus("fastino", customSecretRef),
		GetEngineSecretStatus("contrastive-lm", customSecretRef),
	}
}

// SyncDecisionProvidersFromLocalConfig synchronizes the in-memory decision engine cluster/endpoint
// registry with a persisted LocalConfig (Providers["local"]).
func SyncDecisionProvidersFromLocalConfig(prov config.ProviderConfig) {
	if !prov.Enabled || prov.Regions == nil {
		return
	}
	reg, ok := prov.Regions["local-cluster"]
	if !ok || reg.LocalConfig == nil {
		return
	}
	localCfg := reg.LocalConfig
	for _, ep := range localCfg.DirectEndpoints {
		if !ep.Active {
			continue
		}
		eng := NormalizeEngineName(ep.Engine)
		if eng == "" {
			eng = NormalizeEngineName(ep.ModelName)
		}
		if eng == "" || strings.TrimSpace(ep.URL) == "" {
			continue
		}
		mode := strings.TrimSpace(ep.ConnectionMode)
		if mode == "" {
			if eng == "contrastive-lm" {
				mode = "cluster"
			} else {
				mode = "remote"
			}
		}
		cid := strings.TrimSpace(ep.ClusterID)
		if cid == "" {
			cid = ep.ID
		}
		SetEngineClusterConfig(eng, ep.URL, mode, cid)
	}
	for engKey, dp := range localCfg.DecisionProviders {
		eng := NormalizeEngineName(dp.Engine)
		if eng == "" {
			eng = NormalizeEngineName(engKey)
		}
		if eng == "" {
			continue
		}
		SetEngineClusterConfig(eng, dp.Endpoint, dp.ConnectionMode, dp.ClusterID)
	}
}

// ApplyDecisionProviderToLocalConfig mutates a ProviderConfig ("local") to persist a decision
// engine's endpoint, connection mode, cluster ID, and DirectEndpoint registration for cluster runners.
func ApplyDecisionProviderToLocalConfig(prov *config.ProviderConfig, engine, endpoint, connectionMode, clusterID, secretRef string) {
	norm := NormalizeEngineName(engine)
	if norm == "" || prov == nil {
		return
	}
	if prov.ID == "" {
		prov.ID = config.ProviderLocal
	}
	if prov.DisplayName == "" {
		prov.DisplayName = "Local / Open-Weights Clusters"
	}
	prov.Enabled = true
	if prov.Regions == nil {
		prov.Regions = make(map[string]config.RegionConfig)
	}
	reg, ok := prov.Regions["local-cluster"]
	if !ok {
		reg = config.RegionConfig{
			Code:        "local-cluster",
			Active:      true,
			LocalConfig: &config.LocalConfig{},
		}
	}
	if reg.LocalConfig == nil {
		reg.LocalConfig = &config.LocalConfig{}
	}
	localCfg := reg.LocalConfig
	if localCfg.DecisionProviders == nil {
		localCfg.DecisionProviders = make(map[string]config.DecisionProviderConfig)
	}

	ep := strings.TrimSpace(endpoint)
	mode := strings.ToLower(strings.TrimSpace(connectionMode))
	cid := strings.TrimSpace(clusterID)
	ref := strings.TrimSpace(secretRef)
	if ref == "" {
		ref = DefaultSecretRefForEngine(norm)
	}

	if mode == "local" {
		ep = ""
	} else if mode == "" {
		if ep != "" {
			if cid != "" || norm == "contrastive-lm" {
				mode = "cluster"
			} else {
				mode = "remote"
			}
		} else {
			mode = "local"
		}
	}
	if cid == "" && (mode == "cluster" || mode == "direct") {
		if norm == "contrastive-lm" {
			cid = "clm-cluster-default"
		} else {
			cid = norm + "-cluster-default"
		}
	}

	SetEngineClusterConfig(norm, ep, mode, cid)

	localCfg.DecisionProviders[norm] = config.DecisionProviderConfig{
		Engine:         norm,
		Endpoint:       ep,
		ConnectionMode: mode,
		ClusterID:      cid,
		SecretRef:      ref,
	}

	// Synchronize DirectEndpoints for cluster/direct decision engine runners (especially Contrastive-LM)
	if mode == "local" {
		filtered := make([]config.DirectEndpoint, 0, len(localCfg.DirectEndpoints))
		for _, d := range localCfg.DirectEndpoints {
			dEng := NormalizeEngineName(d.Engine)
			if dEng == "" {
				dEng = NormalizeEngineName(d.ModelName)
			}
			if dEng == norm && (d.Role == "decision_engine" || norm == "contrastive-lm") {
				continue
			}
			filtered = append(filtered, d)
		}
		localCfg.DirectEndpoints = filtered
		prov.Regions["local-cluster"] = reg
		return
	}

	if ep != "" && (norm == "contrastive-lm" || mode == "cluster" || mode == "direct") {
		epID := cid
		if epID == "" {
			epID = norm + "-cluster"
		}
		newDE := config.DirectEndpoint{
			ID:             epID,
			Name:           DisplayNameForEngine(norm) + " (" + epID + ")",
			URL:            ep,
			ModelName:      norm,
			Active:         true,
			Role:           "decision_engine",
			Engine:         norm,
			SecretRef:      ref,
			ConnectionMode: mode,
			ClusterID:      cid,
		}
		updated := false
		for i, d := range localCfg.DirectEndpoints {
			dEng := NormalizeEngineName(d.Engine)
			if dEng == "" {
				dEng = NormalizeEngineName(d.ModelName)
			}
			if d.ID == epID || (dEng == norm && (d.Role == "decision_engine" || norm == "contrastive-lm")) {
				localCfg.DirectEndpoints[i] = newDE
				updated = true
				break
			}
		}
		if !updated {
			localCfg.DirectEndpoints = append(localCfg.DirectEndpoints, newDE)
		}
	}
	if mode == "cluster" && cid != "" {
		hasCluster := false
		for _, c := range localCfg.Clusters {
			if c.ID == cid {
				hasCluster = true
				break
			}
		}
		if !hasCluster {
			localCfg.Clusters = append(localCfg.Clusters, config.LocalCluster{
				ID:            cid,
				Name:          DisplayNameForEngine(norm) + " Cluster (" + cid + ")",
				MaxQueueDepth: 50,
				MaxQueueAge:   30,
				Nodes:         make(map[string]config.Node),
			})
		}
	}
	prov.Regions["local-cluster"] = reg
}

