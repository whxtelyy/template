package push

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/goleak"
	"go.uber.org/mock/gomock"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestHTTPContractAndTrace(t *testing.T) {
	trip, driver := uuid.New(), uuid.New()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	for _, status := range []int{202, 400, 429, 503, 502} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/push" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("invalid request: %s %s", r.Method, r.URL.Path)
				}
				var body struct {
					RequestID   string            `json:"request_id"`
					RecipientID uuid.UUID         `json:"recipient_id"`
					Kind        string            `json:"kind"`
					Data        map[string]string `json:"data"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Kind != "REQUEST_POSITION" || body.RecipientID != driver || body.Data["trip_id"] != trip.String() {
					t.Errorf("body=%+v", body)
				}
				if _, err := uuid.Parse(body.RequestID); err != nil {
					t.Error(err)
				}
				if r.Header.Get("traceparent") == "" {
					t.Error("missing W3C TraceContext")
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			gateway, err := New(server.URL, time.Second, tp)
			require.NoError(t, err)
			defer gateway.Close()
			ctx, span := tp.Tracer("test").Start(context.Background(), "worker")
			err = gateway.RequestPosition(ctx, trip, driver)
			span.End()
			if status == 202 {
				require.NoError(t, err)
			} else {
				var statusErr *StatusError
				require.ErrorAs(t, err, &statusErr)
				require.Equal(t, status, statusErr.Status)
			}
		})
	}
	spans := sr.Ended()
	var clients int
	for _, s := range spans {
		if s.SpanKind() == trace.SpanKindClient {
			clients++
			require.True(t, s.Parent().IsValid())
		}
	}
	require.Equal(t, 5, clients)
}

func TestBodyAlwaysClosed(t *testing.T) {
	for _, status := range []int{202, 400, 429, 503, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			ctrl := gomock.NewController(t)
			rt := NewMockRoundTripper(ctrl)
			body := NewMockReadCloser(ctrl)
			rt.EXPECT().RoundTrip(gomock.Any()).Return(&http.Response{StatusCode: status, Body: body}, nil)
			body.EXPECT().Read(gomock.Any()).Return(0, io.EOF)
			body.EXPECT().Close().Return(nil)
			g := &Gateway{client: &http.Client{Transport: rt}, url: "http://push.test/api/v1/push", timeout: time.Second}
			err := g.RequestPosition(context.Background(), uuid.New(), uuid.New())
			require.Equal(t, status != 202, err != nil)
		})
	}
	t.Run("read error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		rt := NewMockRoundTripper(ctrl)
		body := NewMockReadCloser(ctrl)
		rt.EXPECT().RoundTrip(gomock.Any()).Return(&http.Response{StatusCode: 202, Body: body}, nil)
		body.EXPECT().Read(gomock.Any()).Return(0, io.ErrUnexpectedEOF)
		body.EXPECT().Close().Return(nil)
		g := &Gateway{client: &http.Client{Transport: rt}, url: "http://push.test", timeout: time.Second}
		require.ErrorIs(t, g.RequestPosition(context.Background(), uuid.New(), uuid.New()), io.ErrUnexpectedEOF)
	})
}

func TestTransportTimeoutAndDisconnect(t *testing.T) {
	for _, disconnect := range []bool{true, false} {
		t.Run(map[bool]string{true: "disconnect", false: "5000ms latency exceeds timeout"}[disconnect], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if disconnect {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				timer := time.NewTimer(5 * time.Second)
				defer timer.Stop()
				select {
				case <-r.Context().Done():
				case <-timer.C:
					w.WriteHeader(http.StatusAccepted)
				}
			}))
			defer server.Close()
			tp := sdktrace.NewTracerProvider()
			defer func() { _ = tp.Shutdown(context.Background()) }()
			g, err := New(server.URL, 30*time.Millisecond, tp)
			require.NoError(t, err)
			defer g.Close()
			err = g.RequestPosition(context.Background(), uuid.New(), uuid.New())
			require.Error(t, err)
			var statusErr *StatusError
			require.False(t, errors.As(err, &statusErr))
			if !disconnect {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			}
		})
	}
}
