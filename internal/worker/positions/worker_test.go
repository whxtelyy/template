package positions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/goleak"
	"go.uber.org/mock/gomock"

	domain "github.com/whxtelyy/template/internal/domain/trip"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func testTrip(n int) domain.StaleTrip {
	id := uuid.MustParse(fmt.Sprintf("%032x", n))
	return domain.StaleTrip{ID: id, DriverID: uuid.New(), PositionAt: time.Unix(int64(n), 0).UTC()}
}

func testWorker(t *testing.T, size, workers, batch int) (*Worker, *Mockrepository, *Mockgateway, chan time.Time) {
	t.Helper()
	ctrl := gomock.NewController(t)
	repo, gw, ticker := NewMockrepository(ctrl), NewMockgateway(ctrl), NewMockticker(ctrl)
	ticks := make(chan time.Time)
	ticker.EXPECT().Ticks().Return((<-chan time.Time)(ticks)).AnyTimes()
	ticker.EXPECT().Stop()
	metrics, err := NewMetrics(noop.NewMeterProvider().Meter("test"))
	require.NoError(t, err)
	w, err := New(Config{Workers: workers, QueueSize: size, BatchSize: batch, StaleAfter: time.Minute, QueryTimeout: time.Second, RequestTimeout: time.Second}, repo, gw, ticker, metrics, tracenoop.NewTracerProvider().Tracer("test"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		require.NoError(t, w.Shutdown(ctx))
	})
	return w, repo, gw, ticks
}

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for test event")
		var zero T
		return zero
	}
}

func TestPriorityEvictionCancellationAndDedup(t *testing.T) {
	q := newQueue(3)
	link := trace.SpanContext{}
	for _, n := range []int{30, 10, 20} {
		result, evicted := q.put(testTrip(n), link)
		require.Equal(t, queued, result)
		require.Nil(t, evicted)
	}
	result, evicted := q.put(testTrip(5), link)
	require.Equal(t, queued, result)
	require.Equal(t, testTrip(30).ID, evicted.trip.ID)
	result, _ = q.put(testTrip(40), link)
	require.Equal(t, dropped, result)
	result, _ = q.put(testTrip(10), link)
	require.Equal(t, duplicate, result)
	q.cancel(testTrip(20).ID)
	result, _ = q.put(testTrip(30), link)
	require.Equal(t, queued, result) // eviction released dedup entry
	for _, n := range []int{5, 10, 30} {
		task, ctx := q.take(context.Background())
		require.Equal(t, testTrip(n).ID, task.trip.ID)
		result, _ := q.put(task.trip, link)
		require.Equal(t, duplicate, result) // in-flight too
		q.cancel(task.trip.ID)
		require.ErrorIs(t, ctx.Err(), context.Canceled)
		result, _ = q.put(task.trip, link)
		require.Equal(t, duplicate, result)
		q.done(task)
	}
	require.Empty(t, q.pending)
	require.Empty(t, q.slots)
	result, _ = q.put(testTrip(20), link)
	require.Equal(t, queued, result)
}

func TestEqualTimeOrderingAndScanInvalidation(t *testing.T) {
	q := newQueue(2)
	a, b := testTrip(1), testTrip(2)
	b.PositionAt = a.PositionAt
	q.put(b, trace.SpanContext{})
	q.put(a, trace.SpanContext{})
	first, _ := q.take(context.Background())
	require.Equal(t, a.ID, first.trip.ID)
	q.done(first)
	q.scanStart()
	q.cancel(testTrip(3).ID)
	result, _ := q.put(testTrip(3), trace.SpanContext{})
	require.Equal(t, duplicate, result)
	q.scanEnd()
	result, _ = q.put(testTrip(3), trace.SpanContext{})
	require.Equal(t, queued, result)
	q.abort()
	require.Empty(t, q.pending)
	require.Empty(t, q.slots)
}

func TestScan250InThreeBatches(t *testing.T) {
	w, repo, _, _ := testWorker(t, 300, 1, 100)
	trips := make([]domain.StaleTrip, 250)
	for i := range trips {
		trips[i] = testTrip(i + 1)
	}
	gomock.InOrder(
		repo.EXPECT().StaleTrips(gomock.Any(), (*domain.StaleTrip)(nil), time.Minute, 100).Return(trips[:100], nil),
		repo.EXPECT().StaleTrips(gomock.Any(), &trips[99], time.Minute, 100).Return(trips[100:200], nil),
		repo.EXPECT().StaleTrips(gomock.Any(), &trips[199], time.Minute, 100).Return(trips[200:], nil),
	)
	w.check(context.Background())
	size, _ := w.q.sizes()
	require.Equal(t, 250, size)
	for _, want := range trips {
		got, _ := w.q.take(context.Background())
		require.Equal(t, want.ID, got.trip.ID)
		w.q.done(got)
	}
}

func TestFullStopsBatchLoadingAndNextTickRestarts(t *testing.T) {
	w, repo, _, _ := testWorker(t, 1, 1, 2)
	repo.EXPECT().StaleTrips(gomock.Any(), (*domain.StaleTrip)(nil), time.Minute, 2).Return([]domain.StaleTrip{testTrip(1), testTrip(2)}, nil).Times(2)
	w.check(context.Background())
	w.check(context.Background())
	size, _ := w.q.sizes()
	require.Equal(t, 1, size)
}

func TestScannerSequentialRecoveryAndCancellationDuringRead(t *testing.T) {
	w, repo, _, ticks := testWorker(t, 10, 1, 10)
	entered, release, second := make(chan struct{}), make(chan struct{}), make(chan struct{})
	gomock.InOrder(
		repo.EXPECT().StaleTrips(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(context.Context, *domain.StaleTrip, time.Duration, int) ([]domain.StaleTrip, error) {
			close(entered)
			<-release
			return nil, errors.New("database down")
		}),
		repo.EXPECT().StaleTrips(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(context.Context, *domain.StaleTrip, time.Duration, int) ([]domain.StaleTrip, error) {
			w.Cancel(testTrip(1).ID)
			close(second)
			return []domain.StaleTrip{testTrip(1)}, nil
		}),
	)
	w.Start(context.Background())
	w.Start(context.Background())
	ticks <- time.Now()
	recv(t, entered)
	select {
	case ticks <- time.Now():
		t.Fatal("scanner accepted overlapping tick")
	default:
	}
	close(release)
	ticks <- time.Now()
	recv(t, second)
	// No gateway expectation: the coordinate invalidates the stale SQL result.
}

func TestWorkersFixedConcurrencyDrainAndContinueAfterError(t *testing.T) {
	w, _, gw, _ := testWorker(t, 10, 2, 10)
	entered, release := make(chan uuid.UUID, 6), make(chan struct{})
	var active, maxActive atomic.Int32
	gw.EXPECT().RequestPosition(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, id, _ uuid.UUID) error {
		n := active.Add(1)
		for old := maxActive.Load(); n > old && !maxActive.CompareAndSwap(old, n); old = maxActive.Load() {
		}
		entered <- id
		<-release
		active.Add(-1)
		return errors.New("push unavailable")
	}).Times(6)
	for i := 1; i <= 6; i++ {
		w.q.put(testTrip(i), trace.SpanContext{})
	}
	w.Start(context.Background())
	recv(t, entered)
	recv(t, entered)
	select {
	case <-entered:
		t.Fatal("more than two workers")
	default:
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, w.Shutdown(ctx))
	require.Equal(t, int32(2), maxActive.Load())
	require.Empty(t, w.q.pending)
}

func TestShutdownDeadlineAndParentCancellation(t *testing.T) {
	for _, parentCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "parent"}[parentCancel], func(t *testing.T) {
			w, _, gw, _ := testWorker(t, 2, 1, 10)
			entered := make(chan struct{})
			gw.EXPECT().RequestPosition(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, _, _ uuid.UUID) error { close(entered); <-ctx.Done(); return ctx.Err() })
			w.q.put(testTrip(1), trace.SpanContext{})
			w.q.put(testTrip(2), trace.SpanContext{})
			root, cancelRoot := context.WithCancel(context.Background())
			defer cancelRoot()
			w.Start(root)
			recv(t, entered)
			ctx, cancel := context.WithCancel(context.Background())
			if parentCancel {
				cancelRoot()
			} else {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			}
			err := w.Shutdown(ctx)
			cancel()
			if !parentCancel {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			} else {
				require.NoError(t, err)
			}
			require.Empty(t, w.q.pending)
		})
	}
}

func TestCancelledQueuedTaskNeverSent(t *testing.T) {
	w, _, gw, _ := testWorker(t, 2, 1, 10)
	w.q.put(testTrip(1), trace.SpanContext{})
	w.q.put(testTrip(2), trace.SpanContext{})
	w.Cancel(testTrip(1).ID)
	gw.EXPECT().RequestPosition(gomock.Any(), testTrip(2).ID, gomock.Any()).Return(nil)
	w.Start(context.Background())
	require.NoError(t, w.Shutdown(context.Background()))
}

func TestMetricsAndSpanLinks(t *testing.T) {
	w, repo, gw, _ := testWorker(t, 2, 1, 10)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	metrics, err := NewMetrics(mp.Meter("test"))
	require.NoError(t, err)
	require.NoError(t, metrics.observe(w.q))
	t.Cleanup(metrics.unregister)
	w.metrics = metrics
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	w.tracer = tp.Tracer("test")
	repo.EXPECT().StaleTrips(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return([]domain.StaleTrip{testTrip(2), testTrip(3), testTrip(1), testTrip(4)}, nil)
	w.check(context.Background()) // 3 queued, eviction once, incoming drop once
	w.metrics.RecordSaved(context.Background())
	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &data))
	values := map[string]int64{}
	for _, s := range data.ScopeMetrics {
		for _, m := range s.Metrics {
			if gauge, ok := m.Data.(metricdata.Gauge[int64]); ok {
				values[m.Name] = gauge.DataPoints[0].Value
			}
		}
	}
	require.Equal(t, int64(2), values["tripgo.position_requests.queue.size"])
	require.Zero(t, values["tripgo.position_requests.workers.active"])
	entered, release := make(chan struct{}), make(chan struct{})
	gw.EXPECT().RequestPosition(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(context.Context, uuid.UUID, uuid.UUID) error { close(entered); <-release; return nil })
	w.Cancel(testTrip(2).ID)
	w.Start(context.Background())
	recv(t, entered)
	require.NoError(t, reader.Collect(context.Background(), &data))
	values = map[string]int64{}
	for _, s := range data.ScopeMetrics {
		for _, m := range s.Metrics {
			if gauge, ok := m.Data.(metricdata.Gauge[int64]); ok {
				values[m.Name] = gauge.DataPoints[0].Value
			}
		}
	}
	require.Zero(t, values["tripgo.position_requests.queue.size"])
	require.Equal(t, int64(1), values["tripgo.position_requests.workers.active"])
	close(release)
	require.NoError(t, w.Shutdown(context.Background()))
	spans := sr.Ended()
	require.Len(t, spans, 2)
	require.False(t, spans[1].Parent().IsValid())
	require.Len(t, spans[1].Links(), 1)
	require.Equal(t, spans[0].SpanContext(), spans[1].Links()[0].SpanContext)
	require.NoError(t, reader.Collect(context.Background(), &data))
	counters := map[string]map[string]int64{}
	for _, s := range data.ScopeMetrics {
		for _, m := range s.Metrics {
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				counters[m.Name] = map[string]int64{}
				for _, p := range sum.DataPoints {
					result := ""
					for _, a := range p.Attributes.ToSlice() {
						require.Equal(t, "result", string(a.Key))
						result = a.Value.AsString()
					}
					counters[m.Name][result] += p.Value
				}
			}
			if m.Name == "tripgo.position_requests.duration" {
				require.Equal(t, "s", m.Unit)
				require.Len(t, m.Data.(metricdata.Histogram[float64]).DataPoints, 1)
			}
			if m.Name == "tripgo.stale_checks.rows" {
				require.Equal(t, int64(4), m.Data.(metricdata.Histogram[int64]).DataPoints[0].Sum)
			}
		}
	}
	require.Equal(t, int64(1), counters["tripgo.positions.saved"][""])
	require.Equal(t, int64(3), counters["tripgo.stale_checks"][queued])
	require.Equal(t, int64(1), counters["tripgo.stale_checks"][dropped])
	require.Equal(t, int64(2), counters["tripgo.position_requests"][dropped])
	require.Equal(t, int64(1), counters["tripgo.position_requests"]["success"])
}
