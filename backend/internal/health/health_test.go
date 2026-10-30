package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestHandler(t *testing.T) {
	cases := []struct {
		name       string
		pingErr    error
		wantStatus int
		wantDB     string
	}{
		{"veritabanı ayakta", nil, http.StatusOK, "ok"},
		{"veritabanı düşmüş", errors.New("bağlantı yok"), http.StatusServiceUnavailable, "unreachable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Handler(fakePinger{tc.pingErr})(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

			if rec.Code != tc.wantStatus {
				t.Fatalf("durum %d, beklenen %d", rec.Code, tc.wantStatus)
			}
			var body Response
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Database != tc.wantDB {
				t.Fatalf("database=%q, beklenen %q", body.Database, tc.wantDB)
			}
		})
	}
}
