package positions

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/google/uuid"

	domain "github.com/whxtelyy/template/internal/domain/trip"
	api "github.com/whxtelyy/template/internal/generated"
	"github.com/whxtelyy/template/internal/http/trip/shared"
)

type Handler struct {
	uc     usecase
	logger *slog.Logger
}

func New(uc usecase, logger *slog.Logger) *Handler { return &Handler{uc: uc, logger: logger} }

func decode(r *http.Request, id uuid.UUID) (domain.Position, error) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return domain.Position{}, errors.New("Content-Type must be application/json")
	}
	// Pointers distinguish missing/null fields from valid zero coordinates.
	var body struct {
		Coordinates *struct {
			Latitude  *float64 `json:"latitude"`
			Longitude *float64 `json:"longitude"`
		} `json:"coordinates"`
		RecordedAt *time.Time `json:"recorded_at"`
	}
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(&body); err != nil {
		return domain.Position{}, errors.New("invalid position JSON or timestamp")
	}
	if body.Coordinates == nil || body.Coordinates.Latitude == nil || body.Coordinates.Longitude == nil || body.RecordedAt == nil {
		return domain.Position{}, errors.New("coordinates and recorded_at are required and cannot be null")
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return domain.Position{}, errors.New("expected exactly one JSON object")
	}
	return domain.Position{TripID: id, Coordinates: domain.Coordinates{Latitude: *body.Coordinates.Latitude, Longitude: *body.Coordinates.Longitude}, RecordedAt: *body.RecordedAt}, nil
}

func (h *Handler) Save(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	p, err := decode(r, id)
	if err != nil {
		shared.WriteServiceError(w, r, &domain.ValidationError{Field: "body", Reason: err.Error()}, h.logger)
		return
	}
	saved, err := h.uc.Save(r.Context(), p)
	if err != nil {
		shared.WriteServiceError(w, r, err, h.logger)
		return
	}
	shared.WriteJSON(w, http.StatusCreated, toAPI(saved))
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	positions, err := h.uc.List(r.Context(), id)
	if err != nil {
		shared.WriteServiceError(w, r, err, h.logger)
		return
	}
	result := make([]api.TripPosition, 0, len(positions))
	for _, p := range positions {
		result = append(result, toAPI(p))
	}
	shared.WriteJSON(w, http.StatusOK, api.TripPositionsResponse{Positions: result})
}

func toAPI(p domain.Position) api.TripPosition {
	return api.TripPosition{Id: p.ID, TripId: p.TripID, Coordinates: api.Coordinates{Latitude: p.Coordinates.Latitude, Longitude: p.Coordinates.Longitude}, RecordedAt: p.RecordedAt}
}
