package positions

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	domain "github.com/whxtelyy/template/internal/domain/trip"
)

type Config struct {
	Workers, QueueSize, BatchSize            int
	StaleAfter, QueryTimeout, RequestTimeout time.Duration
}
type Worker struct {
	cfg                      Config
	repo                     repository
	gateway                  gateway
	ticker                   ticker
	q                        *queue
	metrics                  *Metrics
	tracer                   trace.Tracer
	logger                   *slog.Logger
	lifecycle                sync.Mutex
	started                  bool
	stopped                  bool
	stopScanner, stopWorkers context.CancelFunc
	scannerDone              chan struct{}
	workersDone              chan struct{}
}

func New(cfg Config, repo repository, gateway gateway, ticker ticker, metrics *Metrics, tracer trace.Tracer, logger *slog.Logger) (*Worker, error) {
	if cfg.Workers <= 0 || cfg.QueueSize <= 0 || cfg.BatchSize <= 0 || cfg.StaleAfter <= 0 || cfg.QueryTimeout <= 0 || cfg.RequestTimeout <= 0 {
		return nil, fmt.Errorf("position worker configuration must be positive")
	}
	w := &Worker{cfg: cfg, repo: repo, gateway: gateway, ticker: ticker, q: newQueue(cfg.QueueSize), metrics: metrics, tracer: tracer, logger: logger, scannerDone: make(chan struct{}), workersDone: make(chan struct{})}
	if err := metrics.observe(w.q); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Worker) Start(ctx context.Context) {
	w.lifecycle.Lock()
	defer w.lifecycle.Unlock()
	if w.started || w.stopped {
		return
	}
	w.started = true
	scanCtx, stopScan := context.WithCancel(ctx)
	workCtx, stopWork := context.WithCancel(ctx)
	w.stopScanner = stopScan
	w.stopWorkers = stopWork
	var wg sync.WaitGroup
	wg.Add(w.cfg.Workers)
	for range w.cfg.Workers {
		go func() { defer wg.Done(); w.work(workCtx) }()
	}
	go func() { wg.Wait(); w.q.abort(); close(w.workersDone) }()
	go w.scan(scanCtx)
}
func (w *Worker) Cancel(id uuid.UUID) { w.q.cancel(id) }

func (w *Worker) Shutdown(ctx context.Context) error {
	w.lifecycle.Lock()
	if !w.started {
		if !w.stopped {
			w.ticker.Stop()
			w.stopped = true
		}
		w.metrics.unregister()
		w.lifecycle.Unlock()
		return nil
	}
	w.stopScanner()
	w.stopped = true
	w.lifecycle.Unlock()
	select {
	case <-w.scannerDone:
	case <-ctx.Done():
		w.stopWorkers()
		w.q.abort()
		<-w.scannerDone
	}
	// Scanner has stopped and sealed the queue. Workers drain with their own context.
	select {
	case <-w.workersDone:
	case <-ctx.Done():
		w.stopWorkers()
		w.q.abort()
		<-w.workersDone
	}
	w.stopWorkers()
	w.metrics.unregister()
	return ctx.Err()
}

func (w *Worker) scan(ctx context.Context) {
	defer close(w.scannerDone)
	defer w.ticker.Stop()
	defer w.q.close()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.ticker.Ticks():
			if ctx.Err() != nil {
				return
			}
			w.check(ctx)
		}
	}
}

func (w *Worker) check(ctx context.Context) {
	ctx, span := w.tracer.Start(ctx, "positions.check")
	defer span.End()
	w.q.scanStart()
	defer w.q.scanEnd()
	var after *domain.StaleTrip
	var read int64
	defer func() { w.metrics.rows.Record(ctx, read) }()
	for ctx.Err() == nil {
		queryCtx, cancel := context.WithTimeout(ctx, w.cfg.QueryTimeout)
		trips, err := w.repo.StaleTrips(queryCtx, after, w.cfg.StaleAfter, w.cfg.BatchSize)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				w.logger.ErrorContext(ctx, "stale trips scan failed", "error", err)
				span.RecordError(err)
				span.SetStatus(codes.Error, "database error")
			}
			return
		}
		read += int64(len(trips))
		full := false
		for _, trip := range trips {
			if ctx.Err() != nil {
				return
			}
			result, evicted := w.q.put(trip, span.SpanContext())
			w.metrics.stale(ctx, result)
			if evicted != nil {
				w.metrics.drop(ctx)
				w.logger.WarnContext(ctx, "position request evicted")
			}
			if result == dropped {
				full = true
				w.metrics.drop(ctx)
				w.logger.WarnContext(ctx, "position request queue full")
			}
		}
		if full || len(trips) < w.cfg.BatchSize {
			return
		}
		n, _ := w.q.sizes()
		if n == w.cfg.QueueSize {
			return
		}
		last := trips[len(trips)-1]
		after = &last
	}
}

func (w *Worker) work(ctx context.Context) {
	for {
		t, callCtx := w.q.take(ctx)
		if t == nil {
			return
		}
		w.process(callCtx, t)
		w.q.done(t)
	}
}

func (w *Worker) process(ctx context.Context, t *task) {
	// New root, with a link to the asynchronous scanner span.
	ctx, span := w.tracer.Start(ctx, "positions.request", trace.WithNewRoot(), trace.WithLinks(trace.Link{SpanContext: t.link}))
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, w.cfg.RequestTimeout)
	defer cancel()
	start := time.Now()
	err := ctx.Err()
	if err == nil {
		err = w.gateway.RequestPosition(ctx, t.trip.ID, t.trip.DriverID)
	}
	result := "success"
	if err != nil {
		result = "error"
		span.RecordError(err)
		span.SetStatus(codes.Error, "push failed")
		w.logger.WarnContext(ctx, "position request failed", "error", err)
	}
	w.metrics.request(ctx, result, time.Since(start).Seconds())
}
