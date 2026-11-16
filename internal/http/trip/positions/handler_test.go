package positions

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	domain "github.com/whxtelyy/template/internal/domain/trip"
	api "github.com/whxtelyy/template/internal/generated"
)

func TestSaveResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"created", nil, 201, ""},
		{"missing", domain.ErrTripNotFound, 404, "trip_not_found"},
		{"completed", domain.ErrTripCompleted, 409, "trip_completed"},
		{"invalid", &domain.ValidationError{Field: "coordinates", Reason: "invalid"}, 400, "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uc := NewMockusecase(gomock.NewController(t))
			h := New(uc, slog.New(slog.NewTextHandler(io.Discard, nil)))
			id := uuid.New()
			at := time.Now().UTC()
			p := domain.Position{ID: 7, TripID: id, RecordedAt: at, Coordinates: domain.Coordinates{Latitude: 59, Longitude: 30}}
			uc.EXPECT().Save(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, got domain.Position) (domain.Position, error) {
				require.Equal(t, id, got.TripID)
				require.True(t, at.Equal(got.RecordedAt))
				require.Equal(t, p.Coordinates, got.Coordinates)
				return p, tc.err
			})
			body, err := json.Marshal(api.PositionData{Coordinates: api.Coordinates{Latitude: 59, Longitude: 30}, RecordedAt: at})
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "/positions", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.Save(w, req, id)
			require.Equal(t, tc.status, w.Code)
			if tc.code != "" {
				require.Equal(t, "application/problem+json", w.Header().Get("Content-Type"))
				var problem api.Problem
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &problem))
				require.Equal(t, tc.code, problem.Code)
				require.Equal(t, tc.status, int(problem.Status))
			}
		})
	}
}

func TestMalformedPositionJSON(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `[]`, `{"coordinates":null,"recorded_at":"2026-11-12T00:00:00Z"}`,
		`{"coordinates":{"latitude":0},"recorded_at":"2026-11-12T00:00:00Z"}`,
		`{"coordinates":{"latitude":0,"longitude":null},"recorded_at":"2026-11-12T00:00:00Z"}`,
		`{"coordinates":{"latitude":0,"longitude":0,"extra":1},"recorded_at":"2026-11-12T00:00:00Z"}`,
		`{"coordinates":{"latitude":0,"longitude":0},"recorded_at":"bad"}`,
		`{"coordinates":{"latitude":0,"longitude":0},"recorded_at":null}`,
		`{"coordinates":{"latitude":0,"longitude":0},"recorded_at":"2026-11-12T00:00:00Z","extra":1}`,
		`{"coordinates":{"latitude":0,"longitude":0},"recorded_at":"2026-11-12T00:00:00Z"} {}`,
	} {
		t.Run(body, func(t *testing.T) {
			h := New(NewMockusecase(gomock.NewController(t)), slog.Default())
			r := httptest.NewRequest(http.MethodPost, "/positions", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.Save(w, r, uuid.New())
			require.Equal(t, 400, w.Code)
		})
	}
}

func TestListResponses(t *testing.T) {
	id := uuid.New()
	at := time.Now().UTC()
	for _, tc := range []struct {
		name   string
		points []domain.Position
		err    error
		status int
	}{
		{"empty", nil, nil, 200},
		{"ordered", []domain.Position{{ID: 1, TripID: id, RecordedAt: at}, {ID: 2, TripID: id, RecordedAt: at}}, nil, 200},
		{"missing", nil, domain.ErrTripNotFound, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uc := NewMockusecase(gomock.NewController(t))
			uc.EXPECT().List(gomock.Any(), id).Return(tc.points, tc.err)
			w := httptest.NewRecorder()
			New(uc, slog.Default()).List(w, httptest.NewRequest(http.MethodGet, "/positions", nil), id)
			require.Equal(t, tc.status, w.Code)
			if tc.err == nil {
				var body api.TripPositionsResponse
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
				require.NotNil(t, body.Positions)
				require.Len(t, body.Positions, len(tc.points))
				if len(tc.points) > 0 {
					require.Equal(t, int64(1), body.Positions[0].Id)
					require.Equal(t, int64(2), body.Positions[1].Id)
				}
			}
		})
	}
}
