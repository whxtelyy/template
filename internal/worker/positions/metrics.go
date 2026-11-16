package positions

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type Metrics struct {
	saved, checks, requests metric.Int64Counter
	duration                metric.Float64Histogram
	rows                    metric.Int64Histogram
	queue, active           metric.Int64ObservableGauge
	meter                   metric.Meter
	registration            metric.Registration
	once                    sync.Once
}

func NewMetrics(m metric.Meter) (*Metrics, error) {
	x := &Metrics{meter: m}
	var err error
	if x.saved, err = m.Int64Counter("tripgo.positions.saved"); err != nil {
		return nil, err
	}
	if x.checks, err = m.Int64Counter("tripgo.stale_checks"); err != nil {
		return nil, err
	}
	if x.requests, err = m.Int64Counter("tripgo.position_requests"); err != nil {
		return nil, err
	}
	if x.duration, err = m.Float64Histogram("tripgo.position_requests.duration", metric.WithUnit("s")); err != nil {
		return nil, err
	}
	if x.rows, err = m.Int64Histogram("tripgo.stale_checks.rows", metric.WithDescription("Rows read per scan tick")); err != nil {
		return nil, err
	}
	if x.queue, err = m.Int64ObservableGauge("tripgo.position_requests.queue.size"); err != nil {
		return nil, err
	}
	if x.active, err = m.Int64ObservableGauge("tripgo.position_requests.workers.active"); err != nil {
		return nil, err
	}
	return x, nil
}

func (m *Metrics) observe(q *queue) error {
	r, err := m.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		size, active := q.sizes()
		o.ObserveInt64(m.queue, int64(size))
		o.ObserveInt64(m.active, int64(active))
		return nil
	}, m.queue, m.active)
	m.registration = r
	return err
}

func (m *Metrics) unregister() {
	m.once.Do(func() {
		if m.registration != nil {
			_ = m.registration.Unregister()
		}
	})
}
func (m *Metrics) RecordSaved(ctx context.Context) { m.saved.Add(ctx, 1) }
func (m *Metrics) stale(ctx context.Context, result string) {
	m.checks.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

func (m *Metrics) drop(ctx context.Context) {
	m.requests.Add(ctx, 1, metric.WithAttributes(attribute.String("result", dropped)))
}

func (m *Metrics) request(ctx context.Context, result string, seconds float64) {
	attrs := metric.WithAttributes(attribute.String("result", result))
	m.requests.Add(ctx, 1, attrs)
	m.duration.Record(ctx, seconds, attrs)
}
