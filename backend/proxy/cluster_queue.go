package proxy

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"geminirouter/pkg/config"
)

type ClusterQueue struct {
	mu              sync.RWMutex
	highJobs        []*config.QueueJob
	mediumJobs      []*config.QueueJob
	lowJobs         []*config.QueueJob
	pending         map[string]*config.QueueJob // Active lookup for resolutions
	inFlight        map[string]*config.QueueJob // Popped jobs currently executing on a runner
	nodeActiveCount map[string]int              // Active in-flight jobs per runner node ID
	maxSize         int
	maxAge          time.Duration
	stopCh          chan struct{}
	stopOnce        sync.Once
}

func NewClusterQueue(maxSize int, maxAge time.Duration) *ClusterQueue {
	q := &ClusterQueue{
		pending:         make(map[string]*config.QueueJob),
		inFlight:        make(map[string]*config.QueueJob),
		nodeActiveCount: make(map[string]int),
		maxSize:         maxSize,
		maxAge:          maxAge,
		stopCh:          make(chan struct{}),
	}
	go q.startReaper(10 * time.Millisecond)
	return q
}

func tierRank(tier string) int {
	switch tier {
	case "enterprise":
		return 4
	case "premium":
		return 3
	case "standard":
		return 2
	default:
		return 1
	}
}

func insertByTierAndFIFO(jobs *[]*config.QueueJob, job *config.QueueJob) {
	newRank := tierRank(job.Tier)
	s := *jobs
	insertIdx := len(s)
	for i, existing := range s {
		if newRank > tierRank(existing.Tier) {
			insertIdx = i
			break
		}
	}
	if insertIdx == len(s) {
		*jobs = append(s, job)
		return
	}
	s = append(s, nil)
	copy(s[insertIdx+1:], s[insertIdx:])
	s[insertIdx] = job
	*jobs = s
}

// Close stops the background reaper goroutine and releases its ticker resources.
func (q *ClusterQueue) Close() {
	q.stopOnce.Do(func() {
		close(q.stopCh)
	})
}

// GetNodeActiveCount returns the number of currently in-flight jobs assigned to nodeID.
func (q *ClusterQueue) GetNodeActiveCount(nodeID string) int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.nodeActiveCount[nodeID]
}

// CancelJob removes a cancelled job from the priority slices, inFlight, and pending map.
func (q *ClusterQueue) CancelJob(jobID string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	removeFromSlice := func(jobs *[]*config.QueueJob) {
		for i, job := range *jobs {
			if job.ID == jobID {
				*jobs = append((*jobs)[:i], (*jobs)[i+1:]...)
				break
			}
		}
	}

	removeFromSlice(&q.highJobs)
	removeFromSlice(&q.mediumJobs)
	removeFromSlice(&q.lowJobs)
	if job, ok := q.inFlight[jobID]; ok {
		if job.AssignedNodeID != "" && q.nodeActiveCount[job.AssignedNodeID] > 0 {
			q.nodeActiveCount[job.AssignedNodeID]--
		}
		delete(q.inFlight, jobID)
	}
	delete(q.pending, jobID)
}

// GetQueueSnapshot returns a unified snapshot of all in-flight ("processing") and queued ("queued") cluster jobs.
func (q *ClusterQueue) GetQueueSnapshot() []config.QueueSnapshotItem {
	q.mu.RLock()
	defer q.mu.RUnlock()

	now := time.Now()
	var snapshot []config.QueueSnapshotItem

	for _, j := range q.inFlight {
		refTime := j.StartedAt
		if refTime.IsZero() {
			refTime = j.CreatedAt
		}
		snapshot = append(snapshot, config.QueueSnapshotItem{
			AppID:          j.AppID,
			Model:          j.Model,
			Priority:       j.Priority,
			Tier:           j.Tier,
			Status:         "processing",
			ArrivalTime:    j.CreatedAt,
			DurationMs:     now.Sub(refTime).Milliseconds(),
			ClusterID:      j.ClusterID,
			AssignedNodeID: j.AssignedNodeID,
			QueueSource:    "cluster",
		})
	}

	appendQueued := func(jobs []*config.QueueJob) {
		for _, j := range jobs {
			snapshot = append(snapshot, config.QueueSnapshotItem{
				AppID:          j.AppID,
				Model:          j.Model,
				Priority:       j.Priority,
				Tier:           j.Tier,
				Status:         "queued",
				ArrivalTime:    j.CreatedAt,
				DurationMs:     now.Sub(j.CreatedAt).Milliseconds(),
				ClusterID:      j.ClusterID,
				AssignedNodeID: j.AssignedNodeID,
				QueueSource:    "cluster",
			})
		}
	}

	appendQueued(q.highJobs)
	appendQueued(q.mediumJobs)
	appendQueued(q.lowJobs)
	return snapshot
}

// Enqueue inserts a job based on priority and client tier and returns a channel for the result.
func (q *ClusterQueue) Enqueue(job *config.QueueJob) (<-chan config.QueueResult, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.highJobs)+len(q.mediumJobs)+len(q.lowJobs) >= q.maxSize {
		return nil, errors.New("queue capacity exceeded (backpressure)")
	}

	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now()
	}
	job.ResponseChan = make(chan config.QueueResult, 1)
	q.pending[job.ID] = job

	switch job.Priority {
	case "high":
		insertByTierAndFIFO(&q.highJobs, job)
	case "medium":
		insertByTierAndFIFO(&q.mediumJobs, job)
	default:
		insertByTierAndFIFO(&q.lowJobs, job)
	}

	return job.ResponseChan, nil
}

// Poll retrieves the next available job matching supported models and allowed cluster IDs.
func (q *ClusterQueue) Poll(supportedModels []string, allowedClusters []string) (*config.QueueJob, bool) {
	return q.PollForNode("", supportedModels, allowedClusters, 0)
}

// PollForNode retrieves the next available job for a specific runner node while enforcing maxConcurrent in-flight jobs.
func (q *ClusterQueue) PollForNode(nodeID string, supportedModels []string, allowedClusters []string, maxConcurrent int) (*config.QueueJob, bool) {
	q.cleanupExpiredJobs()

	q.mu.Lock()
	defer q.mu.Unlock()

	if nodeID != "" && maxConcurrent > 0 {
		if q.nodeActiveCount[nodeID] >= maxConcurrent {
			return nil, false
		}
	}

	popJob := func(jobs *[]*config.QueueJob) (*config.QueueJob, bool) {
		for i, job := range *jobs {
			// Match ClusterID if agent is bound to specific clusters
			clusterMatch := false
			if len(allowedClusters) == 0 {
				clusterMatch = true
			} else {
				for _, cID := range allowedClusters {
					if job.ClusterID == cID || job.ClusterID == "" {
						clusterMatch = true
						break
					}
				}
			}

			if !clusterMatch {
				continue
			}

			for _, m := range supportedModels {
				if job.Model == m {
					*jobs = append((*jobs)[:i], (*jobs)[i+1:]...)
					job.AssignedNodeID = nodeID
					job.StartedAt = time.Now()
					q.inFlight[job.ID] = job
					if nodeID != "" {
						q.nodeActiveCount[nodeID]++
					}
					return job, true
				}
			}
		}
		return nil, false
	}

	// Maintain Priority Execution (High -> Medium -> Low)
	if job, ok := popJob(&q.highJobs); ok {
		return job, true
	}
	if job, ok := popJob(&q.mediumJobs); ok {
		return job, true
	}
	if job, ok := popJob(&q.lowJobs); ok {
		return job, true
	}

	return nil, false
}

// Resolve completes a job by passing the result back to the proxy connection channel.
func (q *ClusterQueue) Resolve(jobID string, result config.QueueResult) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	job, exists := q.pending[jobID]
	if !exists {
		return false
	}

	if _, ok := q.inFlight[jobID]; ok {
		if job.AssignedNodeID != "" && q.nodeActiveCount[job.AssignedNodeID] > 0 {
			q.nodeActiveCount[job.AssignedNodeID]--
		}
		delete(q.inFlight, jobID)
	}
	delete(q.pending, jobID)
	job.ResponseChan <- result
	close(job.ResponseChan)
	return true
}

func (q *ClusterQueue) effectiveMaxAge(job *config.QueueJob) time.Duration {
	if job != nil && job.MaxAge > 0 {
		return job.MaxAge
	}
	return q.maxAge
}

// cleanupExpiredJobs enforces per-job MaxAge (falling back to queue maxAge) on both queued and in-flight jobs.
func (q *ClusterQueue) cleanupExpiredJobs() {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := time.Now()

	reapList := func(jobs *[]*config.QueueJob) {
		var active []*config.QueueJob
		for _, job := range *jobs {
			ttl := q.effectiveMaxAge(job)
			if ttl > 0 && now.Sub(job.CreatedAt) > ttl {
				if _, ok := q.pending[job.ID]; ok {
					job.ResponseChan <- config.QueueResult{
						StatusCode: http.StatusGatewayTimeout,
						Error:      errors.New("gateway timeout: request exceeded max queue age"),
					}
					close(job.ResponseChan)
					delete(q.pending, job.ID)
				}
			} else {
				active = append(active, job)
			}
		}
		*jobs = active
	}

	reapList(&q.highJobs)
	reapList(&q.mediumJobs)
	reapList(&q.lowJobs)

	for id, job := range q.inFlight {
		ttl := q.effectiveMaxAge(job)
		refTime := job.StartedAt
		if refTime.IsZero() {
			refTime = job.CreatedAt
		}
		if ttl > 0 && now.Sub(refTime) > ttl {
			if job.AssignedNodeID != "" && q.nodeActiveCount[job.AssignedNodeID] > 0 {
				q.nodeActiveCount[job.AssignedNodeID]--
			}
			delete(q.inFlight, id)
			if _, ok := q.pending[id]; ok {
				job.ResponseChan <- config.QueueResult{
					StatusCode: http.StatusGatewayTimeout,
					Error:      errors.New("gateway timeout: request exceeded max queue age"),
				}
				close(job.ResponseChan)
				delete(q.pending, id)
			}
		}
	}

	for id, job := range q.pending {
		ttl := q.effectiveMaxAge(job)
		if ttl > 0 && now.Sub(job.CreatedAt) > ttl {
			if _, ok := q.inFlight[id]; ok {
				if job.AssignedNodeID != "" && q.nodeActiveCount[job.AssignedNodeID] > 0 {
					q.nodeActiveCount[job.AssignedNodeID]--
				}
				delete(q.inFlight, id)
			}
			job.ResponseChan <- config.QueueResult{
				StatusCode: http.StatusGatewayTimeout,
				Error:      errors.New("gateway timeout: request exceeded max queue age"),
			}
			close(job.ResponseChan)
			delete(q.pending, id)
		}
	}
}

// startReaper enforces TTL (MaxQueueAge) on orphaned or hanging jobs.
func (q *ClusterQueue) startReaper(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-q.stopCh:
			return
		case <-ticker.C:
			q.cleanupExpiredJobs()
		}
	}
}

