package config

import (
	"sync"
	"time"
)

// SessionState tracks multi-turn session affinity, sticky routing, and agent task contexts.
type SessionState struct {
	SessionID         string            `json:"session_id"`
	AppID             string            `json:"app_id"`
	Model             string            `json:"model"`
	LockedModel       string            `json:"locked_model,omitempty"`
	LockedTier        string            `json:"locked_tier,omitempty"`
	TurnsSinceUpgrade int               `json:"turns_since_upgrade,omitempty"`
	Policy            string            `json:"policy"` // "lock" (default), "adaptive", "sticky-turn"
	CurrentTask       string            `json:"current_task"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	TurnCount         int               `json:"turn_count"`
	Turn              int               `json:"turn"`
	CreatedAt         time.Time         `json:"created_at"`
	LastActive        time.Time         `json:"last_active"`
	registry          *SessionRegistry
}

// TierRank returns the numeric rank of a complexity tier ("simple"=1, "medium"=2, "complex"=3).
func TierRank(tier string) int {
	switch tier {
	case "simple":
		return 1
	case "medium":
		return 2
	case "complex":
		return 3
	default:
		return 0
	}
}

// SetModel binds or updates the sticky model for the session.
func (s *SessionState) SetModel(model, engine string) {
	s.Model = model
	s.LockedModel = model
	if s.registry != nil {
		s.registry.SetModel(s.SessionID, model)
	}
}

// SetModelAndTier binds or updates the sticky model, tier, and turns-since-upgrade counter for the session.
func (s *SessionState) SetModelAndTier(model, tier string, turnsSinceUpgrade int) {
	s.Model = model
	s.LockedModel = model
	s.LockedTier = tier
	s.TurnsSinceUpgrade = turnsSinceUpgrade
	if s.registry != nil {
		s.registry.SetModelAndTier(s.SessionID, model, tier, turnsSinceUpgrade)
	}
}

// ApplyStickyTurn evaluates the "sticky-turn" session policy for a newly classified turn.
// It keeps the current session model unless the new turn's complexity tier is higher than the
// current locked tier (immediate upgrade), or at least 3 turns have elapsed since the upgrade
// and the new turn is "simple" (controlled downgrade).
func (s *SessionState) ApplyStickyTurn(candidateModel, candidateTier string) (selectedModel, selectedTier string, upgraded bool) {
	if s.Model == "" || s.LockedTier == "" {
		if candidateTier == "" {
			candidateTier = "medium"
		}
		s.SetModelAndTier(candidateModel, candidateTier, 0)
		return candidateModel, candidateTier, false
	}

	currRank := TierRank(s.LockedTier)
	candRank := TierRank(candidateTier)

	if candRank > currRank {
		s.SetModelAndTier(candidateModel, candidateTier, 0)
		return candidateModel, candidateTier, true
	}

	if s.TurnsSinceUpgrade >= 3 && candidateTier == "simple" && currRank > TierRank("simple") {
		s.SetModelAndTier(candidateModel, candidateTier, 0)
		return candidateModel, candidateTier, false
	}

	nextTurns := s.TurnsSinceUpgrade + 1
	s.SetModelAndTier(s.Model, s.LockedTier, nextTurns)
	return s.Model, s.LockedTier, false
}

// SetTask binds or updates the current task for the session.
func (s *SessionState) SetTask(task string) {
	s.CurrentTask = task
	if s.registry != nil {
		s.registry.SetTask(s.SessionID, task)
	}
}

// IncrementTurn increments and returns the turn count for the session.
func (s *SessionState) IncrementTurn() int {
	s.TurnCount++
	s.Turn = s.TurnCount
	if s.registry != nil {
		return s.registry.IncrementTurn(s.SessionID)
	}
	return s.TurnCount
}

// SessionRegistry manages thread-safe multi-turn session state with TTL expiration.
type SessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*SessionState
	ttl      time.Duration
}

// NewSessionRegistry creates a new in-memory session registry with the specified TTL.
func NewSessionRegistry(ttl time.Duration) *SessionRegistry {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &SessionRegistry{
		sessions: make(map[string]*SessionState),
		ttl:      ttl,
	}
}

func (sr *SessionRegistry) pruneExpiredLocked(now time.Time) int {
	pruned := 0
	for id, s := range sr.sessions {
		if now.Sub(s.LastActive) > sr.ttl {
			delete(sr.sessions, id)
			pruned++
		}
	}
	return pruned
}

// GetSession returns the active session if present and not expired.
func (sr *SessionRegistry) GetSession(sessionID string) (*SessionState, bool) {
	if sessionID == "" {
		return nil, false
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()

	now := time.Now()
	sr.pruneExpiredLocked(now)

	s, exists := sr.sessions[sessionID]
	if !exists {
		return nil, false
	}

	s.LastActive = now
	// Return a copy to prevent data races
	cp := *s
	cp.Turn = cp.TurnCount
	cp.LockedModel = cp.Model
	cp.registry = sr
	if s.Metadata != nil {
		cp.Metadata = make(map[string]string, len(s.Metadata))
		for k, v := range s.Metadata {
			cp.Metadata[k] = v
		}
	}
	return &cp, true
}

// GetOrCreateSession retrieves an existing session or initializes a new one.
func (sr *SessionRegistry) GetOrCreateSession(sessionID, appID string, policy ...string) *SessionState {
	if sessionID == "" {
		return nil
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()

	pol := "lock"
	if len(policy) > 0 && policy[0] != "" {
		pol = policy[0]
	}

	now := time.Now()
	sr.pruneExpiredLocked(now)

	if s, exists := sr.sessions[sessionID]; exists {
		s.LastActive = now
		if pol != "" && s.Policy != pol {
			s.Policy = pol
		}
		cp := *s
		cp.Turn = cp.TurnCount
		cp.LockedModel = cp.Model
		cp.registry = sr
		return &cp
	}

	s := &SessionState{
		SessionID:   sessionID,
		AppID:       appID,
		Policy:      pol,
		TurnCount:   0,
		Turn:        0,
		Metadata:    make(map[string]string),
		CreatedAt:   now,
		LastActive:  now,
		registry:    sr,
	}
	sr.sessions[sessionID] = s

	cp := *s
	cp.registry = sr
	return &cp
}

// SetModel binds or updates the sticky model for a given session.
func (sr *SessionRegistry) SetModel(sessionID, model string) {
	if sessionID == "" || model == "" {
		return
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()

	if s, exists := sr.sessions[sessionID]; exists {
		s.Model = model
		s.LockedModel = model
		s.LastActive = time.Now()
	}
}

// SetModelAndTier binds or updates the sticky model, tier, and turnsSinceUpgrade counter for a given session.
func (sr *SessionRegistry) SetModelAndTier(sessionID, model, tier string, turnsSinceUpgrade int) {
	if sessionID == "" || model == "" {
		return
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()

	if s, exists := sr.sessions[sessionID]; exists {
		s.Model = model
		s.LockedModel = model
		s.LockedTier = tier
		s.TurnsSinceUpgrade = turnsSinceUpgrade
		s.LastActive = time.Now()
	}
}

// SetTask binds or updates the current task for a given session.
func (sr *SessionRegistry) SetTask(sessionID, task string) {
	if sessionID == "" {
		return
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()

	if s, exists := sr.sessions[sessionID]; exists {
		s.CurrentTask = task
		s.LastActive = time.Now()
	}
}

// IncrementTurn increments and returns the turn count for the session.
func (sr *SessionRegistry) IncrementTurn(sessionID string) int {
	if sessionID == "" {
		return 1
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()

	if s, exists := sr.sessions[sessionID]; exists {
		s.TurnCount++
		s.LastActive = time.Now()
		return s.TurnCount
	}
	return 1
}

// PruneExpired purges any sessions exceeding the TTL.
func (sr *SessionRegistry) PruneExpired() int {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	return sr.pruneExpiredLocked(time.Now())
}

// ListSessions returns a slice of all active, non-expired sessions.
func (sr *SessionRegistry) ListSessions() []*SessionState {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	now := time.Now()
	var list []*SessionState
	for id, s := range sr.sessions {
		if now.Sub(s.LastActive) > sr.ttl {
			delete(sr.sessions, id)
			continue
		}
		cp := *s
		cp.Turn = cp.TurnCount
		cp.LockedModel = cp.Model
		list = append(list, &cp)
	}
	return list
}

// DeleteSession removes a specific session by ID.
func (sr *SessionRegistry) DeleteSession(sessionID string) bool {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	if _, exists := sr.sessions[sessionID]; exists {
		delete(sr.sessions, sessionID)
		return true
	}
	return false
}

// Clear removes all active sessions.
func (sr *SessionRegistry) Clear() {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.sessions = make(map[string]*SessionState)
}
