// Package webhook dış sistemlerle (ödeme sağlayıcı, kargo firması) giden ve
// gelen HTTP çağrılarını HMAC-SHA256 ile imzalar ve doğrular.
//
// İmza zaman damgasını da kapsar; eski bir isteğin yeniden oynatılması
// (replay) tolerans süresi dışında reddedilir. Webhook uçları kullanıcı
// oturumu (JWT) yerine bu imzayla yetkilendirilir.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"
)

const (
	HeaderTimestamp = "X-Webhook-Timestamp"
	HeaderSignature = "X-Webhook-Signature"
	Tolerance       = 5 * time.Minute
)

var (
	ErrMissing   = errors.New("imza başlıkları eksik")
	ErrStale     = errors.New("imza zaman damgası tolerans dışında")
	ErrSignature = errors.New("imza geçersiz")
)

func Sign(secret []byte, ts int64, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(strconv.FormatInt(ts, 10)))
	m.Write([]byte{'.'})
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// SetHeaders isteği şimdiki zamanla imzalar.
func SetHeaders(h http.Header, secret []byte, body []byte, now time.Time) {
	ts := now.Unix()
	h.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	h.Set(HeaderSignature, Sign(secret, ts, body))
}

// Verify imzayı sabit zamanlı karşılaştırır ve zaman damgasını denetler.
func Verify(secret []byte, h http.Header, body []byte, now time.Time) error {
	tsRaw, sig := h.Get(HeaderTimestamp), h.Get(HeaderSignature)
	if tsRaw == "" || sig == "" {
		return ErrMissing
	}
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return ErrMissing
	}
	if d := now.Sub(time.Unix(ts, 0)); d > Tolerance || d < -Tolerance {
		return ErrStale
	}
	if !hmac.Equal([]byte(Sign(secret, ts, body)), []byte(sig)) {
		return ErrSignature
	}
	return nil
}
