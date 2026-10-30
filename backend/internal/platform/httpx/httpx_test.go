package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestError_UsesStandardShape(t *testing.T) {
	rec := httptest.NewRecorder()
	Error(rec, http.StatusForbidden, CodeForbidden, "yetkin yok")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("durum kodu %d", rec.Code)
	}
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != CodeForbidden || body.Error.Message != "yetkin yok" {
		t.Fatalf("gövde yanlış: %+v", body)
	}
}

func TestDecode_RejectsUnknownFieldsAndTrailingData(t *testing.T) {
	type in struct{ Name string }
	cases := map[string]bool{
		`{"Name":"a"}`:         true,
		`{"Name":"a","x":1}`:   false,
		`{"Name":"a"} {"x":1}`: false,
		`not json`:             false,
	}
	for body, ok := range cases {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		var v in
		err := Decode(httptest.NewRecorder(), r, &v)
		if (err == nil) != ok {
			t.Errorf("%s: err=%v, başarı beklentisi=%v", body, err, ok)
		}
	}
}

func TestRequestID_KeepsIncomingOrGenerates(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(HeaderRequestID, "abc")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if seen != "abc" || rec.Header().Get(HeaderRequestID) != "abc" {
		t.Fatalf("gelen kimlik korunmadı: %q", seen)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if len(seen) != 16 || rec.Header().Get(HeaderRequestID) != seen {
		t.Fatalf("kimlik üretilmedi: %q", seen)
	}
}

func TestRecover_ReturnsJSON500(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := Recover(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), CodeInternal) {
		t.Fatalf("panik yakalanmadı: %d %s", rec.Code, rec.Body.String())
	}
}

func TestTimeout_SetsDeadline(t *testing.T) {
	var deadline time.Time
	h := Timeout(time.Second)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		deadline, _ = r.Context().Deadline()
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if deadline.IsZero() || time.Until(deadline) > time.Second {
		t.Fatalf("süre sınırı konmadı: %v", deadline)
	}
}

func TestCORS_OnlyAllowedOrigins(t *testing.T) {
	h := CORS([]string{"http://ok"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for origin, allowed := range map[string]bool{"http://ok": true, "http://evil": false} {
		r := httptest.NewRequest(http.MethodOptions, "/", nil)
		r.Header.Set("Origin", origin)
		r.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		got := rec.Header().Get("Access-Control-Allow-Origin") == origin
		if got != allowed || rec.Code != http.StatusNoContent {
			t.Errorf("%s: izin=%v kod=%d", origin, got, rec.Code)
		}
	}
}

func TestAccessLog_WritesStatusWithoutHeaders(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	h := AccessLog(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	r := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(context.Background())
	r.Header.Set("Authorization", "Bearer gizli")
	h.ServeHTTP(httptest.NewRecorder(), r)

	if !strings.Contains(buf.String(), `"status":418`) || strings.Contains(buf.String(), "gizli") {
		t.Fatalf("erişim logu yanlış: %s", buf.String())
	}
}

func TestIsUUID(t *testing.T) {
	for s, want := range map[string]bool{
		"123e4567-e89b-12d3-a456-426614174000": true,
		"123E4567-E89B-12D3-A456-426614174000": true,
		"123e4567e89b12d3a456426614174000":     false,
		"123e4567-e89b-12d3-a456-42661417400g": false,
		"123e4567-e89b-12d3-a456_426614174000": false,
		"":                                     false,
	} {
		if IsUUID(s) != want {
			t.Errorf("IsUUID(%q) != %v", s, want)
		}
	}
}
