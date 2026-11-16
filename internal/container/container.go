package container

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/whxtelyy/template/internal/config"
	"github.com/whxtelyy/template/internal/gateway/push"
	api "github.com/whxtelyy/template/internal/generated"
	httpapi "github.com/whxtelyy/template/internal/http"
	"github.com/whxtelyy/template/internal/http/middleware"
	"github.com/whxtelyy/template/internal/infra/postgres"
	"github.com/whxtelyy/template/internal/infra/postgres/transaction"
	"github.com/whxtelyy/template/internal/infra/telemetry"
	repository "github.com/whxtelyy/template/internal/repository/trip"
	"github.com/whxtelyy/template/internal/usecase/trip/create"
	"github.com/whxtelyy/template/internal/usecase/trip/finish"
	"github.com/whxtelyy/template/internal/usecase/trip/get"
	positionsuc "github.com/whxtelyy/template/internal/usecase/trip/positions"
	"github.com/whxtelyy/template/internal/worker/positions"
)

type App struct {
	Server    *http.Server
	Logger    *slog.Logger
	pool      *pgxpool.Pool
	telemetry *telemetry.Providers
	worker    *positions.Worker
	push      *push.Gateway
}

func (a *App) Close(ctx context.Context) {
	if err := a.worker.Shutdown(ctx); err != nil {
		a.Logger.Warn("position workers shutdown deadline", "error", err)
	}
	a.push.Close()
	a.pool.Close()
	if err := a.telemetry.Shutdown(ctx); err != nil {
		a.Logger.Error("telemetry shutdown failed", "error", err)
	}
}

func New(ctx context.Context, cfg config.Config) (*App, error) {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	logger := slog.New(telemetry.NewTraceHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))).With("service", "trip-service")
	providers, err := telemetry.New(ctx, logger)
	if err != nil {
		return nil, err
	}
	shutdownTelemetry := func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
		defer cancel()
		_ = providers.Shutdown(closeCtx)
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.Database.URL)
	if err != nil {
		shutdownTelemetry()
		return nil, fmt.Errorf("parse database configuration: %w", err)
	}
	poolCfg.MaxConns = cfg.Database.MaxConns
	poolCfg.MinConns = cfg.Database.MinConns
	poolCfg.MaxConnLifetime = cfg.Database.MaxConnLifetime
	poolCfg.ConnConfig.ConnectTimeout = cfg.Database.ConnectTimeout
	queryTracer, err := postgres.NewQueryTracer(otelpgx.NewTracer(otelpgx.WithTracerProvider(providers.Trace), otelpgx.WithMeterProvider(noop.NewMeterProvider())), providers.Metric.Meter("trip-service/db"))
	if err != nil {
		shutdownTelemetry()
		return nil, err
	}
	poolCfg.ConnConfig.Tracer = queryTracer
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		shutdownTelemetry()
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, cfg.Database.ConnectTimeout)
	err = pool.Ping(pingCtx)
	cancel()
	if err != nil {
		pool.Close()
		shutdownTelemetry()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	repo := repository.NewTripRepository(pool, cfg.Database.QueryTimeout)
	txManager := transaction.NewManager(pool, pgx.ReadCommitted, cfg.Database.QueryTimeout, logger)
	pushGateway, err := push.New(cfg.Positions.PushURL, cfg.Positions.RequestTimeout, providers.Trace)
	if err != nil {
		pool.Close()
		shutdownTelemetry()
		return nil, err
	}
	positionMetrics, err := positions.NewMetrics(providers.Metric.Meter("trip-service/positions"))
	if err != nil {
		pushGateway.Close()
		pool.Close()
		shutdownTelemetry()
		return nil, err
	}
	ticker := positions.Ticker{Ticker: time.NewTicker(cfg.Positions.CheckInterval)}
	worker, err := positions.New(positions.Config{Workers: cfg.Positions.Workers, QueueSize: cfg.Positions.QueueSize, BatchSize: cfg.Positions.BatchSize, StaleAfter: cfg.Positions.StaleAfter, QueryTimeout: cfg.Database.QueryTimeout, RequestTimeout: cfg.Positions.RequestTimeout}, repo, pushGateway, ticker, positionMetrics, providers.Trace.Tracer("trip-service/positions"), logger)
	if err != nil {
		ticker.Stop()
		pushGateway.Close()
		pool.Close()
		shutdownTelemetry()
		return nil, err
	}
	handler := httpapi.NewHandler(create.New(repo, txManager, providers), get.New(repo), finish.New(repo, txManager, providers), positionsuc.New(repo, txManager, positions.Clock{}, worker, positionMetrics), pool, logger, cfg.Database.QueryTimeout)
	metrics, err := middleware.Metrics(providers.Metric.Meter("trip-service/http"))
	if err != nil {
		_ = worker.Shutdown(context.WithoutCancel(ctx))
		pushGateway.Close()
		pool.Close()
		shutdownTelemetry()
		return nil, err
	}
	router := chi.NewRouter()
	router.Use(middleware.Recover(logger), middleware.Tracing(providers.Trace), middleware.Logging(logger), metrics)
	httpHandler := api.HandlerWithOptions(handler, api.ChiServerOptions{BaseRouter: router, ErrorHandlerFunc: handler.GeneratedErrorHandler})
	server := &http.Server{
		Addr: cfg.HTTPAddr, Handler: httpHandler,
		ReadTimeout: cfg.HTTPReadTimeout, ReadHeaderTimeout: cfg.HTTPReadHeaderTimeout,
		WriteTimeout: cfg.HTTPWriteTimeout, IdleTimeout: cfg.HTTPIdleTimeout,
	}
	// Signal cancellation is handled in main, after HTTP has drained.
	worker.Start(context.WithoutCancel(ctx))
	return &App{Server: server, Logger: logger, pool: pool, telemetry: providers, worker: worker, push: pushGateway}, nil
}
