package postgres

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.uber.org/mock/gomock"
)

func TestCollectionName(t *testing.T) {
	for _, tc := range []struct{ sql, want string }{
		{"SELECT id FROM trips WHERE id=$1", "trips"},
		{"INSERT INTO trip_status_history (trip_id) VALUES ($1)", "trip_status_history"},
		{"UPDATE trip_idempotency_keys SET trip_id=$1", "trip_idempotency_keys"},
		{"SELECT 1", "unknown"},
	} {
		if got := collectionName(tc.sql); got != tc.want {
			t.Errorf("collectionName(%q) = %q, want %q", tc.sql, got, tc.want)
		}
	}
}

func TestQueryDurationBuckets(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	delegate := NewMockQueryTracer(gomock.NewController(t))
	delegate.EXPECT().TraceQueryStart(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context { return ctx })
	delegate.EXPECT().TraceQueryEnd(gomock.Any(), gomock.Any(), gomock.Any())
	tracer, err := NewQueryTracer(delegate, provider.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT id FROM trips"})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.ScopeMetrics) != 1 || len(data.ScopeMetrics[0].Metrics) != 1 {
		t.Fatalf("data=%+v", data)
	}
	metric := data.ScopeMetrics[0].Metrics[0]
	histogram, ok := metric.Data.(metricdata.Histogram[float64])
	if !ok || len(histogram.DataPoints) != 1 {
		t.Fatalf("metric=%+v", metric)
	}
	point := histogram.DataPoints[0]
	if metric.Name != "db.client.operation.duration" || metric.Unit != "s" || !reflect.DeepEqual(point.Bounds, []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}) {
		t.Fatalf("metric=%+v", metric)
	}
}
