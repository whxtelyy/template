package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	domain "github.com/whxtelyy/template/internal/domain/trip"
	api "github.com/whxtelyy/template/internal/generated"
	"github.com/whxtelyy/template/internal/usecase/trip/create"
)

func testHandler(t *testing.T) (*Handler, *MockcreateUseCase, *MockgetUseCase, *MockfinishUseCase) {
	t.Helper()
	ctrl := gomock.NewController(t)
	c, g, f := NewMockcreateUseCase(ctrl), NewMockgetUseCase(ctrl), NewMockfinishUseCase(ctrl)
	ready := NewMockreadiness(ctrl)
	return NewHandler(c, g, f, NewMockpositionsUseCase(ctrl), ready, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second), c, g, f
}

func TestCreateTripResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		replay bool
		status int
	}{
		{"created", false, http.StatusCreated}, {"replayed", true, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, c, _, _ := testHandler(t)
			id := uuid.New()
			c.EXPECT().Execute(gomock.Any(), gomock.Any()).Return(create.Result{Trip: domain.Trip{ID: id, UserID: uuid.New(), DriverID: uuid.New(), Status: domain.StatusActive}, Replayed: tc.replay}, nil)
			body, _ := json.Marshal(api.TripData{UserId: uuid.New(), DriverId: uuid.New(), StartPoint: api.Coordinates{}, EndPoint: api.Coordinates{}, Price: 100})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/trips", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.CreateTrip(w, req, api.CreateTripParams{})
			if w.Code != tc.status || w.Header().Get("Location") != "/api/v1/trips/"+id.String() {
				t.Fatalf("status %d, headers %v, body %s", w.Code, w.Header(), w.Body.String())
			}
		})
	}
}

func TestCreateTripMalformedBody(t *testing.T) {
	h, _, _, _ := testHandler(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/trips", strings.NewReader("{"))
	req.Header.Set("Content-Type", "application/json")
	h.CreateTrip(w, req, api.CreateTripParams{})
	if w.Code != http.StatusBadRequest || w.Header().Get("Content-Type") != "application/problem+json" || !strings.Contains(w.Body.String(), `"code":"invalid_request"`) {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
}

func TestGetTripErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"missing", domain.ErrTripNotFound, http.StatusNotFound, "trip_not_found"},
		{"internal", errors.New("secret database credential"), http.StatusInternalServerError, "internal_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, g, _ := testHandler(t)
			id := uuid.New()
			g.EXPECT().Execute(gomock.Any(), id).Return(domain.Trip{}, tc.err)
			w := httptest.NewRecorder()
			h.GetTrip(w, httptest.NewRequest(http.MethodGet, "/api/v1/trips/"+id.String(), nil), id)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) || strings.Contains(w.Body.String(), "secret database") {
				t.Fatalf("response: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestFinishTripCompleted(t *testing.T) {
	h, _, _, f := testHandler(t)
	id := uuid.New()
	f.EXPECT().Execute(gomock.Any(), id).Return(domain.Trip{}, domain.ErrTripCompleted)
	w := httptest.NewRecorder()
	h.FinishTrip(w, httptest.NewRequest(http.MethodPost, "/api/v1/trips/"+id.String()+"/finish", nil).WithContext(context.Background()), id)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"trip_completed"`) {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
}

func TestCreateContentType(t *testing.T) {
	for _, tc := range []struct {
		name, contentType string
		valid             bool
	}{
		{"json", "application/json", true},
		{"charset", "application/json; charset=utf-8", true},
		{"missing", "", false},
		{"text", "text/plain", false},
		{"malformed", "application/json; charset", false},
		{"invalid", "not a media type", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, uc, _, _ := testHandler(t)
			body, err := json.Marshal(api.TripData{UserId: uuid.New(), DriverId: uuid.New(), Price: 100})
			if err != nil {
				t.Fatal(err)
			}
			want := http.StatusBadRequest
			if tc.valid {
				want = http.StatusCreated
				uc.EXPECT().Execute(gomock.Any(), gomock.Any()).Return(create.Result{Trip: domain.Trip{ID: uuid.New()}}, nil)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/trips", bytes.NewReader(body))
			req.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			h.CreateTrip(w, req, api.CreateTripParams{})
			if w.Code != want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if !tc.valid && (w.Header().Get("Content-Type") != "application/problem+json" || !strings.Contains(w.Body.String(), `"code":"invalid_request"`)) {
				t.Fatalf("body=%s", w.Body.String())
			}
		})
	}
}

func TestCreateJSONStructure(t *testing.T) {
	valid := `{"user_id":"11111111-1111-4111-8111-111111111111","driver_id":"22222222-2222-4222-8222-222222222222","start_point":{"latitude":1,"longitude":2},"end_point":{"latitude":3,"longitude":4},"price":100}`
	for _, body := range []string{
		"{", "null", valid + "{}", strings.Replace(valid, `"price":100`, `"price":null`, 1),
		strings.Replace(valid, `"price":100`, `"price":"100"`, 1), strings.Replace(valid, `,"price":100`, "", 1),
		strings.Replace(valid, `"price":100`, `"extra":1,"price":100`, 1),
		strings.Replace(valid, `"latitude":1`, `"latitude":null`, 1), strings.Replace(valid, `"latitude":1`, `"latitude":"1"`, 1),
	} {
		t.Run(body, func(t *testing.T) {
			h, _, _, _ := testHandler(t)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/trips", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.CreateTrip(w, req, api.CreateTripParams{})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCreateBusinessValidationFromUseCase(t *testing.T) {
	h, uc, _, _ := testHandler(t)
	uc.EXPECT().Execute(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, req create.Request) (create.Result, error) {
		if req.StartPoint.Latitude != 91 || req.Price != -1 {
			t.Fatalf("request=%+v", req)
		}
		return create.Result{}, &domain.ValidationError{Field: "start_point.latitude", Reason: "must be between -90 and 90"}
	})
	body, err := json.Marshal(api.TripData{UserId: uuid.New(), DriverId: uuid.New(), StartPoint: api.Coordinates{Latitude: 91}, Price: -1})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/trips", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.CreateTrip(w, req, api.CreateTripParams{})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"invalid_request"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
