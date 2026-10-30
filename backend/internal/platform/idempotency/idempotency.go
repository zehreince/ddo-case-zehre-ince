// Package idempotency durum değiştiren kritik POST'lar için Idempotency-Key
// desteği sağlar.
//
//   - Anahtar yoksa istek olağan şekilde işlenir.
//   - İlk istekte anahtar "processing" olarak kaydedilir, yanıt saklanır.
//   - Aynı anahtar + aynı istek: işlem tekrar yapılmaz, saklanan yanıt döner.
//   - Aynı anahtar + farklı istek gövdesi/yolu: 422 (anahtar başka iş için kullanılmış).
//   - İlk istek hâlâ sürüyorsa: 409; istemci kısa süre sonra tekrar dener.
//   - 5xx yanıtlar saklanmaz; geçici hatadan sonra aynı anahtarla yeniden denenebilir.
//
// Anahtarlar kullanıcı başınadır; iki kullanıcının aynı anahtarı çakışmaz.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/platform/httpx"
)

const (
	Header         = "Idempotency-Key"
	ReplayedHeader = "Idempotent-Replayed"
	maxKeyLen      = 200
	maxBodyBytes   = 1 << 20
)

// UserFunc istekteki kimliği doğrulanmış kullanıcıyı döner.
type UserFunc func(ctx context.Context) (userID string, ok bool)

func Middleware(pool *pgxpool.Pool, userFrom UserFunc, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(Header)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			userID, ok := userFrom(r.Context())
			if !ok {
				httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "kimlik doğrulama gerekli")
				return
			}
			if len(key) > maxKeyLen {
				httpx.ValidationError(w, map[string]string{Header: "en fazla 200 karakter"})
				return
			}

			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
			if err != nil {
				httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "gövde okunamadı")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			hash := requestHash(r.Method, r.URL.Path, body)

			claimed, err := claim(r.Context(), pool, userID, key, r.Method, r.URL.Path, hash)
			if err != nil {
				httpx.Internal(w, r, log, err)
				return
			}
			if !claimed {
				replay(w, r, pool, log, userID, key, hash)
				return
			}

			rec := &recorder{header: http.Header{}, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			// Yanıtı saklamak isteğin kendi bağlamından bağımsızdır: istemci
			// bağlantıyı kesse bile işlem yapıldıysa kaydı tamamlanmalı.
			ctx := context.WithoutCancel(r.Context())
			if rec.status >= 500 {
				_, err = pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE user_id = $1::uuid AND key = $2`, userID, key)
			} else {
				_, err = pool.Exec(ctx, `
					UPDATE idempotency_keys
					SET status = 'completed', response_code = $3, response_body = $4, completed_at = now()
					WHERE user_id = $1::uuid AND key = $2`, userID, key, rec.status, rec.body.Bytes())
			}
			if err != nil {
				log.ErrorContext(ctx, "idempotency kaydı güncellenemedi", "error", err)
			}
			rec.writeTo(w)
		})
	}
}

func requestHash(method, path string, body []byte) []byte {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(body)
	return h.Sum(nil)
}

// claim anahtarı bu istek adına ayırır; anahtar zaten varsa false döner.
func claim(ctx context.Context, pool *pgxpool.Pool, userID, key, method, path string, hash []byte) (bool, error) {
	tag, err := pool.Exec(ctx, `
		INSERT INTO idempotency_keys (user_id, key, method, path, request_hash, status)
		VALUES ($1::uuid, $2, $3, $4, $5, 'processing')
		ON CONFLICT (user_id, key) DO NOTHING`, userID, key, method, path, hash)
	return tag.RowsAffected() == 1, err
}

func replay(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, log *slog.Logger, userID, key string, hash []byte) {
	var (
		storedHash []byte
		status     string
		code       *int
		body       []byte
	)
	err := pool.QueryRow(r.Context(), `
		SELECT request_hash, status, response_code, response_body
		FROM idempotency_keys WHERE user_id = $1::uuid AND key = $2`, userID, key).
		Scan(&storedHash, &status, &code, &body)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// İlk istek 5xx ile bitip kaydı sildi; istemci yeniden denemeli.
		httpx.Error(w, http.StatusConflict, "idempotency_retry", "önceki istek tamamlanamadı, yeniden deneyin")
	case err != nil:
		httpx.Internal(w, r, log, err)
	case !bytes.Equal(storedHash, hash):
		httpx.Error(w, http.StatusUnprocessableEntity, "idempotency_key_reused",
			"bu Idempotency-Key farklı bir istek için kullanılmış")
	case status != "completed" || code == nil:
		httpx.Error(w, http.StatusConflict, "idempotency_in_progress", "aynı anahtarla bir istek hâlâ işleniyor")
	default:
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set(ReplayedHeader, "true")
		w.WriteHeader(*code)
		_, _ = w.Write(body)
	}
}

// recorder yanıtı tamponlar; saklandıktan sonra gerçek yazara aktarılır.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.body.Write(b)
}

func (r *recorder) writeTo(w http.ResponseWriter) {
	for k, v := range r.header {
		w.Header()[k] = v
	}
	w.WriteHeader(r.status)
	_, _ = w.Write(r.body.Bytes())
}
