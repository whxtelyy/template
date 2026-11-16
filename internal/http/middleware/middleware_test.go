package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/mock/gomock"

	api "github.com/whxtelyy/template/internal/generated"
)

const (
	healthRoute    = "/health"
	unmatchedRoute = "unmatched"
	tripRoute      = "/api/v1/trips/{tripId}"
)

func TestRecover(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := Recover(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("test panic") }))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, healthRoute, nil))
	var body api.Problem
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusInternalServerError || body.Code != "internal_error" || !strings.Contains(logs.String(), "stack") {
		t.Fatalf("status=%d body=%s logs=%s", w.Code, w.Body.String(), logs.String())
	}
}

func TestRouteUsesTemplate(t *testing.T) {
	for _, tc := range []struct {
		method, path, route string
		status              int
	}{
		{http.MethodGet, healthRoute, healthRoute, 200},
		{http.MethodGet, "/api/v1/trips/5cb72c04-7650-45c9-a79b-bcdba0631e0c", tripRoute, 200},
		{http.MethodPost, "/api/v1/trips/5cb72c04-7650-45c9-a79b-bcdba0631e0c/finish", "/api/v1/trips/{tripId}/finish", 200},
		{http.MethodGet, "/api/v1/trips/bad-uuid", tripRoute, 400},
		{http.MethodGet, "/unknown/5cb72c04-7650-45c9-a79b-bcdba0631e0c", unmatchedRoute, 404},
		{http.MethodDelete, healthRoute, unmatchedRoute, 405},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
			metrics, err := Metrics(mp.Meter("test"))
			if err != nil {
				t.Fatal(err)
			}
			sr := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
			router := chi.NewRouter()
			router.Use(Tracing(tp), metrics)
			server := NewMockServerInterface(gomock.NewController(t))
			ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
			if tc.status == 200 {
				switch tc.route {
				case healthRoute:
					server.EXPECT().Health(gomock.Any(), gomock.Any()).Do(ok)
				case tripRoute:
					server.EXPECT().GetTrip(gomock.Any(), gomock.Any(), gomock.Any()).Do(func(w http.ResponseWriter, r *http.Request, _ any) { ok(w, r) })
				default:
					server.EXPECT().FinishTrip(gomock.Any(), gomock.Any(), gomock.Any()).Do(func(w http.ResponseWriter, r *http.Request, _ any) { ok(w, r) })
				}
			}
			h := api.HandlerWithOptions(server, api.ChiServerOptions{BaseRouter: router})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			spans := sr.Ended()
			if len(spans) != 1 || spans[0].Name() != tc.method+" "+tc.route {
				t.Fatalf("spans=%v", spans)
			}
			assertHTTPMetric(t, reader, tc.route, tc.status)
		})
	}
}

func assertHTTPMetric(t *testing.T, reader *sdkmetric.ManualReader, route string, status int) {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != "http.server.request.duration" {
				continue
			}
			histogram, ok := metric.Data.(metricdata.Histogram[float64])
			if !ok || len(histogram.DataPoints) != 1 {
				t.Fatalf("metric=%+v", metric)
			}
			point := histogram.DataPoints[0]
			if !reflect.DeepEqual(point.Bounds, []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}) {
				t.Fatalf("bounds=%v", point.Bounds)
			}
			gotRoute, _ := point.Attributes.Value(attribute.Key("http.route"))
			gotStatus, _ := point.Attributes.Value(attribute.Key("http.response.status_code"))
			if gotRoute.AsString() != route || gotStatus.AsInt64() != int64(status) || metric.Unit != "s" {
				t.Fatalf("metric=%+v", metric)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("HTTP duration metric missing")
	}
}

func TestPanicMarksSpanAndMetricAsError(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	metrics, err := Metrics(mp.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Use(Recover(logger), Tracing(tp), Logging(logger), metrics)
	router.Get("/panic/{id}", func(http.ResponseWriter, *http.Request) { panic("original panic") })
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/panic/private-id", nil))
	spans := sr.Ended()
	if w.Code != 500 || len(spans) != 1 || spans[0].Status().Code != codes.Error || spans[0].Name() != "GET /panic/{id}" {
		t.Fatalf("status=%d spans=%v", w.Code, spans)
	}
	events := spans[0].Events()
	if len(events) != 1 || events[0].Name != "exception" {
		t.Fatalf("events=%v", events)
	}
	if strings.Count(logs.String(), "request handled") != 1 || !strings.Contains(logs.String(), "original panic") {
		t.Fatalf("logs=%s", logs.String())
	}
	assertHTTPMetric(t, reader, "/panic/{id}", 500)
}

func TestRouteWithoutRouter(t *testing.T) {
	if got := Route(httptest.NewRequest(http.MethodGet, "/private-id", nil)); got != unmatchedRoute {
		t.Fatalf("route=%s", got)
	}
}
