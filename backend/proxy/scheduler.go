package proxy

import (
	"container/heap"
	"context"
	"errors"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"geminirouter/pkg/config"
)

// QueueItem represents a request waiting in the priority queue.
type QueueItem struct {
	PriorityValue int       // Numeric priority (higher runs first)
	ArrivalTime   time.Time // For FIFO fallback
	Ctx           context.Context
	Done          chan struct{}
	Index         int // Managed by container/heap

	// Meta info for dashboard/monitoring
	AppID       string    `json:"app_id"`
	Model       string    `json:"model"`
	Priority    string    `json:"priority"`
	Tier        string    `json:"tier"`
	Status      string    `json:"status"` // "queued" or "processing"
}

type PriorityQueue []*QueueItem

func (pq PriorityQueue) Len() int { return len(pq) }
func (pq PriorityQueue) Less(i, j int) bool {
	if pq[i].PriorityValue != pq[j].PriorityValue {
		return pq[i].PriorityValue > pq[j].PriorityValue // Max-heap: higher value comes first
	}
	return pq[i].ArrivalTime.Before(pq[j].ArrivalTime) // FIFO fallback
}
func (pq PriorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].Index = i
	pq[j].Index = j
}
func (pq *PriorityQueue) Push(x interface{}) {
	n := len(*pq)
	item := x.(*QueueItem)
	item.Index = n
	*pq = append(*pq, item)
}
func (pq *PriorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.Index = -1
	*pq = old[0 : n-1]
	return item
}

type RequestScheduler struct {
	mu                  sync.Mutex
	pq                  PriorityQueue
	maxQueueSize        int
	activeLimit         int
	activeCount         int
	activeItems         map[*QueueItem]bool
	activeCountPerModel map[string]int
	overloadedModels    map[string]time.Time
}

func NewRequestScheduler(maxQueueSize, activeLimit int) *RequestScheduler {
	s := &RequestScheduler{
		pq:                  make(PriorityQueue, 0),
		maxQueueSize:        maxQueueSize,
		activeLimit:         activeLimit,
		activeItems:         make(map[*QueueItem]bool),
		activeCountPerModel: make(map[string]int),
		overloadedModels:    make(map[string]time.Time),
	}
	heap.Init(&s.pq)
	return s
}

// isModelOverloaded checks if a model has overloaded/429 status and is in cooldown.
func (s *RequestScheduler) isModelOverloaded(model string) bool {
	until, exists := s.overloadedModels[model]
	if !exists {
		return false
	}
	if time.Now().After(until) {
		delete(s.overloadedModels, model)
		return false
	}
	return true
}

// Enqueue inserts a request. If queue is full, it returns an error immediately (backpressure).
func (s *RequestScheduler) Enqueue(ctx context.Context, appID, priority, tier, model string) (*QueueItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.pq) >= s.maxQueueSize {
		return nil, errors.New("queue capacity exceeded")
	}

	priorityVal := 0
	switch priority {
	case "high":
		priorityVal += 300
	case "medium":
		priorityVal += 200
	case "low":
		priorityVal += 100
	}
	switch tier {
	case "premium":
		priorityVal += 3
	case "standard":
		priorityVal += 2
	case "free":
		priorityVal += 1
	}

	item := &QueueItem{
		PriorityValue: priorityVal,
		ArrivalTime:   time.Now(),
		Ctx:           ctx,
		Done:          make(chan struct{}),
		Index:         -1,
		AppID:         appID,
		Model:         model,
		Priority:      priority,
		Tier:          tier,
		Status:        "queued",
	}

	isOverloaded := s.isModelOverloaded(model)

	// If global capacity is available AND the model is not overloaded, run immediately
	if s.activeCount < s.activeLimit && !isOverloaded {
		s.activeCount++
		s.activeCountPerModel[model]++
		item.Status = "processing"
		s.activeItems[item] = true
		close(item.Done)
		return item, nil
	}

	// If model IS overloaded, but there are zero active requests, let 1 request through to test recovery
	if isOverloaded && s.activeCount < s.activeLimit && s.activeCountPerModel[model] < 1 {
		s.activeCount++
		s.activeCountPerModel[model]++
		item.Status = "processing"
		s.activeItems[item] = true
		close(item.Done)
		return item, nil
	}

	heap.Push(&s.pq, item)

	// Spawn cleaner to remove item on context cancellation
	go func() {
		<-ctx.Done()
		s.Remove(item)
	}()

	return item, nil
}

// Release decrements active counts and triggers a dispatch.
func (s *RequestScheduler) Release(item *QueueItem) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if item != nil {
		delete(s.activeItems, item)
		if s.activeCountPerModel[item.Model] > 0 {
			s.activeCountPerModel[item.Model]--
		}
	}

	s.activeCount--
	s.dispatch()
}

func (s *RequestScheduler) Remove(item *QueueItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item.Index >= 0 && item.Index < len(s.pq) {
		heap.Remove(&s.pq, item.Index)
		// If we removed an item, trigger a dispatch to see if another can fill the active slot
		s.dispatch()
	}
}

func (s *RequestScheduler) dispatch() {
	for s.activeCount < s.activeLimit {
		var targetItem *QueueItem
		var targetIdx = -1

		// Find the highest priority dispatchable item
		for i, item := range s.pq {
			isOverloaded := s.isModelOverloaded(item.Model)
			modelActive := s.activeCountPerModel[item.Model]

			canDispatch := false
			if !isOverloaded {
				canDispatch = true
			} else if modelActive < 1 {
				canDispatch = true
			}

			if canDispatch {
				if targetItem == nil || s.pq.Less(i, targetIdx) {
					targetItem = item
					targetIdx = i
				}
			}
		}

		if targetItem == nil {
			break
		}

		heap.Remove(&s.pq, targetIdx)

		s.activeCount++
		s.activeCountPerModel[targetItem.Model]++
		targetItem.Status = "processing"
		s.activeItems[targetItem] = true
		close(targetItem.Done)
	}
}

// ReportRequestStatus registers whether an upstream request succeeded or failed with 429.
func (s *RequestScheduler) ReportRequestStatus(model string, statusCode int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if statusCode == http.StatusTooManyRequests {
		s.overloadedModels[model] = time.Now().Add(15 * time.Second)
		log.Printf("[Scheduler] Model %s reported 429. Marking as overloaded.", model)
	} else if statusCode == http.StatusOK || (statusCode >= 200 && statusCode < 300) {
		if _, exists := s.overloadedModels[model]; exists {
			delete(s.overloadedModels, model)
			log.Printf("[Scheduler] Model %s succeeded. Clearing overloaded status.", model)
			s.dispatch()
		}
	}
}

// QueueSnapshotItem represents a simplified structure for dashboard JSON output.
// GetQueueStatus returns a snapshot of all active and queued requests.
func (s *RequestScheduler) GetQueueStatus() []config.QueueSnapshotItem {
	s.mu.Lock()
	defer s.mu.Unlock()

	var result []config.QueueSnapshotItem

	// 1. Add active items
	for item := range s.activeItems {
		duration := time.Since(item.ArrivalTime)
		result = append(result, config.QueueSnapshotItem{
			AppID:       item.AppID,
			Model:       item.Model,
			Priority:    item.Priority,
			Tier:        item.Tier,
			Status:      "processing",
			ArrivalTime: item.ArrivalTime,
			DurationMs:  duration.Milliseconds(),
		})
	}

	// 2. Extract queued items
	var queuedItems []*QueueItem
	for _, item := range s.pq {
		queuedItems = append(queuedItems, item)
	}

	// Sort queued items in priority order
	sort.Slice(queuedItems, func(i, j int) bool {
		if queuedItems[i].PriorityValue != queuedItems[j].PriorityValue {
			return queuedItems[i].PriorityValue > queuedItems[j].PriorityValue
		}
		return queuedItems[i].ArrivalTime.Before(queuedItems[j].ArrivalTime)
	})

	// Add sorted queued items
	for _, item := range queuedItems {
		duration := time.Since(item.ArrivalTime)
		result = append(result, config.QueueSnapshotItem{
			AppID:       item.AppID,
			Model:       item.Model,
			Priority:    item.Priority,
			Tier:        item.Tier,
			Status:      "queued",
			ArrivalTime: item.ArrivalTime,
			DurationMs:  duration.Milliseconds(),
		})
	}

	return result
}
