package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	api "github.com/whxtelyy/template/internal/generated"
	createhttp "github.com/whxtelyy/template/internal/http/trip/create"
	finishhttp "github.com/whxtelyy/template/internal/http/trip/finish"
	gethttp "github.com/whxtelyy/template/internal/http/trip/get"
	positionshttp "github.com/whxtelyy/template/internal/http/trip/positions"
	"github.com/whxtelyy/template/internal/http/trip/shared"
)

type Handler struct {
	create       *createhttp.Handler
	get          *gethttp.Handler
	finish       *finishhttp.Handler
	positions    *positionshttp.Handler
	db           readiness
	logger       *slog.Logger
	readyTimeout time.Duration
}

func NewHandler(create createUseCase, get getUseCase, finish finishUseCase, positions positionsUseCase, db readiness, logger *slog.Logger, readyTimeout time.Duration) *Handler {
	return &Handler{create: createhttp.New(create, logger), get: gethttp.New(get, logger), finish: finishhttp.New(finish, logger), positions: positionshttp.New(positions, logger), db: db, logger: logger, readyTimeout: readyTimeout}
}

func (h *Handler) CreateTripPosition(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	h.positions.Save(w, r, id)
}

func (h *Handler) ListTripPositions(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	h.positions.List(w, r, id)
}

var _ api.ServerInterface = (*Handler)(nil)

func (h *Handler) GeneratedErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	shared.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "Invalid request", "Request parameter is invalid", "https://tripgo.example/problems/invalid-request")
	h.logger.InfoContext(r.Context(), "invalid request parameter", "error", err.Error())
}

func (h *Handler) CreateTrip(w http.ResponseWriter, r *http.Request, params api.CreateTripParams) {
	h.create.Handle(w, r, params)
}

func (h *Handler) GetTrip(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	h.get.Handle(w, r, id)
}

func (h *Handler) FinishTrip(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	h.finish.Handle(w, r, id)
}

func (h *Handler) Health(w http.ResponseWriter, _ *http.Request) {
	shared.WriteJSON(w, http.StatusOK, api.HealthResponse{Status: api.HealthResponseStatus("ok")})
}

func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.readyTimeout)
	defer cancel()
	if err := h.db.Ping(ctx); err != nil {
		shared.WriteJSON(w, http.StatusServiceUnavailable, api.HealthResponse{Status: api.HealthResponseStatus("unavailable")})
		return
	}
	shared.WriteJSON(w, http.StatusOK, api.HealthResponse{Status: api.HealthResponseStatus("ok")})
}
