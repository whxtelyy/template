package positions

import (
	"container/heap"
	"context"
	"sync"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"

	domain "github.com/whxtelyy/template/internal/domain/trip"
)

const (
	queued    = "queued"
	duplicate = "duplicate"
	dropped   = "dropped"
)

type task struct {
	trip   domain.StaleTrip
	link   trace.SpanContext
	index  int // -1 means in-flight; entry remains in pending until completion.
	cancel context.CancelFunc
}
type taskHeap []*task

func (h taskHeap) Len() int { return len(h) }
func older(a, b domain.StaleTrip) bool {
	if a.PositionAt.Equal(b.PositionAt) {
		return a.ID.String() < b.ID.String()
	}
	return a.PositionAt.Before(b.PositionAt)
}
func (h taskHeap) Less(i, j int) bool { return older(h[i].trip, h[j].trip) }
func (h taskHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h *taskHeap) Push(x any)        { t := x.(*task); t.index = len(*h); *h = append(*h, t) }
func (h *taskHeap) Pop() any {
	old := *h
	n := len(old) - 1
	t := old[n]
	old[n] = nil
	*h = old[:n]
	t.index = -1
	return t
}

// heap owns tasks; slots is the mandatory bounded buffered channel. Exactly one
// token per queued task is added/removed with the heap under mu. Workers wait on
// cond, never consume tokens outside mu, so cancellation cannot lose wakeups.
type queue struct {
	mu          sync.Mutex
	cond        *sync.Cond
	heap        taskHeap
	slots       chan struct{}
	pending     map[uuid.UUID]*task
	closed      bool
	reading     bool
	invalidated map[uuid.UUID]struct{} // only for a currently running SQL scan
	active      int
}

func newQueue(size int) *queue {
	q := &queue{slots: make(chan struct{}, size), pending: make(map[uuid.UUID]*task)}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *queue) put(trip domain.StaleTrip, link trace.SpanContext) (result string, evicted *task) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return dropped, nil
	}
	if _, ok := q.invalidated[trip.ID]; ok {
		return duplicate, nil
	}
	if _, ok := q.pending[trip.ID]; ok {
		return duplicate, nil
	}
	if len(q.heap) == cap(q.slots) {
		newest := 0
		for i := 1; i < len(q.heap); i++ {
			if older(q.heap[newest].trip, q.heap[i].trip) {
				newest = i
			}
		}
		if !older(trip, q.heap[newest].trip) {
			return dropped, nil
		}
		evicted = heap.Remove(&q.heap, newest).(*task)
		delete(q.pending, evicted.trip.ID)
		<-q.slots
	}
	t := &task{trip: trip, link: link}
	heap.Push(&q.heap, t)
	q.slots <- struct{}{}
	q.pending[trip.ID] = t
	q.cond.Signal()
	return queued, evicted
}

func (q *queue) take(ctx context.Context) (*task, context.Context) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.heap) == 0 && !q.closed && ctx.Err() == nil {
		q.cond.Wait()
	}
	if ctx.Err() != nil || len(q.heap) == 0 {
		return nil, nil
	}
	t := heap.Pop(&q.heap).(*task)
	<-q.slots
	callCtx, cancel := context.WithCancel(ctx)
	t.cancel = cancel
	q.active++
	return t, callCtx
}

func (q *queue) done(t *task) {
	q.mu.Lock()
	defer q.mu.Unlock()
	t.cancel()
	delete(q.pending, t.trip.ID)
	q.active--
}

func (q *queue) cancel(id uuid.UUID) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.reading {
		q.invalidated[id] = struct{}{}
	}
	if t, ok := q.pending[id]; ok {
		if t.index >= 0 {
			heap.Remove(&q.heap, t.index)
			<-q.slots
			delete(q.pending, id)
		} else {
			t.cancel()
		}
	}
}

func (q *queue) scanStart() {
	q.mu.Lock()
	q.reading = true
	q.invalidated = make(map[uuid.UUID]struct{})
	q.mu.Unlock()
}
func (q *queue) scanEnd() { q.mu.Lock(); q.reading = false; q.invalidated = nil; q.mu.Unlock() }
func (q *queue) close()   { q.mu.Lock(); q.closed = true; q.cond.Broadcast(); q.mu.Unlock() }
func (q *queue) abort() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	for _, t := range q.pending {
		if t.cancel != nil {
			t.cancel()
		}
	}
	for len(q.heap) > 0 {
		t := heap.Pop(&q.heap).(*task)
		<-q.slots
		delete(q.pending, t.trip.ID)
	}
	q.cond.Broadcast()
}

func (q *queue) sizes() (int, int) { q.mu.Lock(); defer q.mu.Unlock(); return len(q.heap), q.active }
