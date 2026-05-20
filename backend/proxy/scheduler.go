package proxy

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"time"
)

// QueueItem represents a request waiting in the priority queue.
type QueueItem struct {
	PriorityValue int       // Numeric priority (higher runs first)
	ArrivalTime   time.Time // For FIFO fallback
	Ctx           context.Context
	Done          chan struct{}
	Index         int // Managed by container/heap
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
	mu           sync.Mutex
	pq           PriorityQueue
	maxQueueSize int
	activeLimit  int
	activeCount  int
}

func NewRequestScheduler(maxQueueSize, activeLimit int) *RequestScheduler {
	s := &RequestScheduler{
		pq:           make(PriorityQueue, 0),
		maxQueueSize: maxQueueSize,
		activeLimit:  activeLimit,
	}
	heap.Init(&s.pq)
	return s
}

// Enqueue inserts a request. If queue is full, it returns an error immediately (backpressure).
func (s *RequestScheduler) Enqueue(ctx context.Context, priority, tier string) (*QueueItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.pq) >= s.maxQueueSize {
		return nil, errors.New("queue capacity exceeded")
	}

	// If capacity is available, execute immediately
	if s.activeCount < s.activeLimit {
		s.activeCount++
		item := &QueueItem{Done: make(chan struct{}), Index: -1}
		close(item.Done)
		return item, nil
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
	}

	heap.Push(&s.pq, item)

	// Spawn cleaner to remove item on context cancellation
	go func() {
		<-ctx.Done()
		s.Remove(item)
	}()

	return item, nil
}

func (s *RequestScheduler) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	for s.activeCount < s.activeLimit && len(s.pq) > 0 {
		item := heap.Pop(&s.pq).(*QueueItem)
		s.activeCount++
		close(item.Done)
	}
}
