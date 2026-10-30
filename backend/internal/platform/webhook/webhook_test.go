package webhook

import (
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestSignAndVerify(t *testing.T) {
	secret := []byte("gizli")
	body := []byte(`{"payment_id":"p1","status":"succeeded"}`)
	now := time.Now()
	h := http.Header{}
	SetHeaders(h, secret, body, now)

	if err := Verify(secret, h, body, now); err != nil {
		t.Fatalf("geçerli imza reddedildi: %v", err)
	}
	cases := map[string]struct {
		secret []byte
		body   []byte
		now    time.Time
		want   error
	}{
		"başka anahtar":       {[]byte("baska"), body, now, ErrSignature},
		"değiştirilmiş gövde": {secret, []byte(`{"payment_id":"p1","status":"failed"}`), now, ErrSignature},
		"eski istek (replay)": {secret, body, now.Add(10 * time.Minute), ErrStale},
		"gelecekten":          {secret, body, now.Add(-10 * time.Minute), ErrStale},
	}
	for name, tc := range cases {
		if err := Verify(tc.secret, h, tc.body, tc.now); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, beklenen %v", name, err, tc.want)
		}
	}
	if err := Verify(secret, http.Header{}, body, now); !errors.Is(err, ErrMissing) {
		t.Errorf("başlıksız: %v", err)
	}
	bad := http.Header{}
	bad.Set(HeaderTimestamp, "abc")
	bad.Set(HeaderSignature, Sign(secret, 1, body))
	if err := Verify(secret, bad, body, now); !errors.Is(err, ErrMissing) {
		t.Errorf("bozuk zaman damgası: %v", err)
	}
	// Zaman damgası imzanın parçası: başlıktaki zamanı değiştirmek imzayı bozar.
	h.Set(HeaderTimestamp, strconv.FormatInt(now.Unix()+1, 10))
	if err := Verify(secret, h, body, now); !errors.Is(err, ErrSignature) {
		t.Errorf("zaman damgası değiştirildi: %v", err)
	}
}
