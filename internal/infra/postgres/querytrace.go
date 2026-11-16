package postgres

import (
	"context"
	"strings"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type (
	queryKey  struct{}
	queryData struct {
		start                 time.Time
		operation, collection string
	}
)

// QueryTracer delegates spans to otelpgx and records the required collection-aware DB duration metric.
type QueryTracer struct {
	Delegate pgx.QueryTracer
	Duration metric.Float64Histogram
}

//go:generate go tool mockgen -destination=mocks_test.go -package=postgres github.com/jackc/pgx/v5 QueryTracer

func NewQueryTracer(delegate pgx.QueryTracer, meter metric.Meter) (*QueryTracer, error) {
	duration, err := meter.Float64Histogram("db.client.operation.duration", metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10))
	if err != nil {
		return nil, err
	}
	return &QueryTracer{Delegate: delegate, Duration: duration}, nil
}

func (t *QueryTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	operation := otelpgx.SQLOperationName(ctx, data.SQL)
	collection := collectionName(data.SQL)
	ctx = t.Delegate.TraceQueryStart(ctx, conn, data)
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("db.operation.name", operation), attribute.String("db.collection.name", collection))
	return context.WithValue(ctx, queryKey{}, queryData{start: time.Now(), operation: operation, collection: collection})
}

func (t *QueryTracer) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if details, ok := ctx.Value(queryKey{}).(queryData); ok {
		attrs := []attribute.KeyValue{attribute.String("db.operation.name", details.operation), attribute.String("db.collection.name", details.collection)}
		if data.Err != nil {
			attrs = append(attrs, attribute.String("error.type", "database_error"))
			trace.SpanFromContext(ctx).SetAttributes(attribute.String("error.type", "database_error"))
		}
		t.Duration.Record(ctx, time.Since(details.start).Seconds(), metric.WithAttributes(attrs...))
	}
	t.Delegate.TraceQueryEnd(ctx, conn, data)
}

func collectionName(query string) string {
	fields := strings.Fields(strings.ToUpper(query))
	for i, token := range fields {
		if (token == "FROM" || token == "INTO" || token == "UPDATE") && i+1 < len(fields) {
			name := strings.Trim(fields[i+1], `"(),;`)
			if name == "TRIPS" || name == "TRIP_POSITIONS" || name == "TRIP_STATUS_HISTORY" || name == "TRIP_IDEMPOTENCY_KEYS" {
				return strings.ToLower(name)
			}
		}
	}
	return "unknown"
}
