package proxy

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"geminirouter/pkg/config"
)

type ClusterQueue struct {
	mu         sync.RWMutex
	highJobs   []*config.QueueJob
	mediumJobs []*config.QueueJob
	lowJobs    []*config.QueueJob
	pending    map[string]*config.QueueJob // Active lookup for resolutions
	maxSize    int
	maxAge     time.Duration
}

func NewClusterQueue(maxSize int, maxAge time.Duration) *ClusterQueue {
	q := &ClusterQueue{
		pending: make(map[string]*config.QueueJob),
		maxSize: maxSize,
		maxAge:  maxAge,
	}
	go q.startReaper(10 * time.Millisecond)
	return q
}

// Enqueue inserts a job based on priority and returns a channel for the result.
func (q *ClusterQueue) Enqueue(job *config.QueueJob) (<-chan config.QueueResult, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.highJobs)+len(q.mediumJobs)+len(q.lowJobs) >= q.maxSize {
		return nil, errors.New("queue capacity exceeded (backpressure)")
	}

	job.ResponseChan = make(chan config.QueueResult, 1)
	q.pending[job.ID] = job

	switch job.Priority {
	case "high":
		q.highJobs = append(q.highJobs, job)
	case "medium":
		q.mediumJobs = append(q.mediumJobs, job)
	default:
		q.lowJobs = append(q.lowJobs, job)
	}

	return job.ResponseChan, nil
}

// Poll retrieves the next available job matching supported models and allowed cluster IDs.
func (q *ClusterQueue) Poll(supportedModels []string, allowedClusters []string) (*config.QueueJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	popJob := func(jobs *[]*config.QueueJob) (*config.QueueJob, bool) {
		for i, job := range *jobs {
			// Match ClusterID if agent is bound to specific clusters
			clusterMatch := false
			if len(allowedClusters) == 0 {
				// If agent has no administrative mappings, it defaults to allowing unassigned or global jobs
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
					// Remove item from slice
					*jobs = append((*jobs)[:i], (*jobs)[i+1:]...)
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

	delete(q.pending, jobID)
	job.ResponseChan <- result
	close(job.ResponseChan)
	return true
}

// startReaper enforces TTL (MaxQueueAge) on orphaned or hanging jobs.
func (q *ClusterQueue) startReaper(interval time.Duration) {
	ticker := time.NewTicker(interval)
	for range ticker.C {
		q.mu.Lock()
		now := time.Now()

		reapList := func(jobs *[]*config.QueueJob) {
			var active []*config.QueueJob
			for _, job := range *jobs {
				if now.Sub(job.CreatedAt) > q.maxAge {
					job.ResponseChan <- config.QueueResult{
						StatusCode: http.StatusGatewayTimeout,
						Error:      errors.New("gateway timeout: request exceeded max queue age"),
					}
					close(job.ResponseChan)
					delete(q.pending, job.ID)
				} else {
					active = append(active, job)
				}
			}
			*jobs = active
		}

		reapList(&q.highJobs)
		reapList(&q.mediumJobs)
		reapList(&q.lowJobs)
		q.mu.Unlock()
	}
}
