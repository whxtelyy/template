package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type Gateway struct {
	client    *http.Client
	transport *http.Transport
	url       string
	timeout   time.Duration
}
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return fmt.Sprintf("push service HTTP status %d", e.Status) }

func New(base string, timeout time.Duration, provider trace.TracerProvider) (*Gateway, error) {
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || timeout <= 0 {
		return nil, fmt.Errorf("invalid push URL or timeout")
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: timeout, KeepAlive: timeout}).DialContext,
		MaxIdleConns: 100, MaxIdleConnsPerHost: 100, IdleConnTimeout: timeout, TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout,
	}
	client := &http.Client{
		Timeout: timeout, Transport: otelhttp.NewTransport(transport, otelhttp.WithTracerProvider(provider), otelhttp.WithPropagators(propagation.TraceContext{})),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &Gateway{client: client, transport: transport, url: strings.TrimRight(base, "/") + "/api/v1/push", timeout: timeout}, nil
}

func (g *Gateway) RequestPosition(ctx context.Context, tripID, driverID uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	body, err := json.Marshal(struct {
		RequestID   string            `json:"request_id"`
		RecipientID uuid.UUID         `json:"recipient_id"`
		Kind        string            `json:"kind"`
		Data        map[string]string `json:"data"`
	}{RequestID: uuid.NewString(), RecipientID: driverID, Kind: "REQUEST_POSITION", Data: map[string]string{"trip_id": tripID.String()}})
	if err != nil {
		return fmt.Errorf("encode position request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create push request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("push transport: %w", err)
	}
	defer response.Body.Close()
	// Bound draining; large/untrusted responses must not consume unbounded memory.
	if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10)); err != nil {
		return fmt.Errorf("read push response: %w", err)
	}
	if response.StatusCode != http.StatusAccepted {
		return &StatusError{Status: response.StatusCode}
	}
	return nil
}
func (g *Gateway) Close() { g.transport.CloseIdleConnections() }
