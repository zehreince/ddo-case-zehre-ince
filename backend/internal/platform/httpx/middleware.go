package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"
)

type ctxKey int

const requestIDKey ctxKey = iota

const HeaderRequestID = "X-Request-ID"

func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// RequestID gelen X-Request-ID'yi korur, yoksa üretir; yanıta da yazar.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if id == "" || len(id) > 64 {
			var b [8]byte
			_, _ = rand.Read(b[:])
			id = hex.EncodeToString(b[:])
		}
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// Timeout her isteğe bir bağlam süresi koyar; veritabanı çağrıları bunu izler.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Recover panikleri yakalar, loglar ve standart 500 gövdesi döner.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					// ErrAbortHandler net/http'nin bağlantıyı kesme sinyalidir; yutulmamalı.
					if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
						panic(rec)
					}
					log.ErrorContext(r.Context(), "panic", "panic", rec, "request_id", RequestIDFrom(r.Context()))
					Error(w, http.StatusInternalServerError, CodeInternal, "beklenmeyen bir hata oluştu")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// StatusRecorder yanıt kodunu loglama ve metrik için yakalar.
type StatusRecorder struct {
	http.ResponseWriter
	Status int
}

func (s *StatusRecorder) WriteHeader(code int) {
	s.Status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *StatusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// AccessLog her isteği tek bir JSON satırı olarak loglar. Başlıklar ve gövde loglanmaz.
func AccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &StatusRecorder{ResponseWriter: w, Status: http.StatusOK}
			next.ServeHTTP(rec, r)
			log.InfoContext(r.Context(), "http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.Status,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", RequestIDFrom(r.Context()),
			)
		})
	}
}

// CORS yalnızca izinli kökenlere yanıt başlığı ekler.
func CORS(origins []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && slices.Contains(origins, origin) {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Request-ID")
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				h.Set("Access-Control-Expose-Headers", "X-Request-ID")
				h.Add("Vary", "Origin")
			}
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
