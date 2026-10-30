package idempotency

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"carsihub/internal/platform/dbtest"
	fx "carsihub/internal/platform/testfixture"
)

type ctxUser struct{}

func setup(t *testing.T, handler http.HandlerFunc) (http.Handler, string) {
	pool := dbtest.New(t)
	user := fx.User(t, pool, "customer")
	mw := Middleware(pool, func(ctx context.Context) (string, bool) {
		u, ok := ctx.Value(ctxUser{}).(string)
		return u, ok
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return mw(handler), user
}

func call(h http.Handler, user, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/orders", strings.NewReader(body))
	if key != "" {
		r.Header.Set(Header, key)
	}
	if user != "" {
		r = r.WithContext(context.WithValue(r.Context(), ctxUser{}, user))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestSameKeyReplaysFirstResponse(t *testing.T) {
	var calls atomic.Int64
	h, user := setup(t, func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"call":`+strconv.FormatInt(n, 10)+`}`)
	})

	first := call(h, user, "k1", `{"a":1}`)
	second := call(h, user, "k1", `{"a":1}`)
	if calls.Load() != 1 {
		t.Fatalf("işlem bir kez yapılmalı, %d kez yapıldı", calls.Load())
	}
	if second.Code != http.StatusCreated || second.Body.String() != first.Body.String() || second.Header().Get(ReplayedHeader) != "true" {
		t.Fatalf("aynı yanıt dönmeli: %d %s", second.Code, second.Body.String())
	}
	// Farklı anahtar yeni işlemdir.
	call(h, user, "k2", `{"a":1}`)
	if calls.Load() != 2 {
		t.Fatal("farklı anahtar işlemi yeniden yapmalı")
	}
}

func TestSameKeyDifferentBodyIsRejected(t *testing.T) {
	h, user := setup(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	call(h, user, "k", `{"a":1}`)
	rec := call(h, user, "k", `{"a":2}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "idempotency_key_reused") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestServerErrorsAreNotStored(t *testing.T) {
	var calls atomic.Int64
	h, user := setup(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	if rec := call(h, user, "k", `{}`); rec.Code != http.StatusInternalServerError {
		t.Fatal(rec.Code)
	}
	if rec := call(h, user, "k", `{}`); rec.Code != http.StatusCreated || calls.Load() != 2 {
		t.Fatalf("5xx sonrası yeniden denenebilmeli: %d calls=%d", rec.Code, calls.Load())
	}
}

func TestConcurrentSameKeyRunsOnce(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	h, user := setup(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		<-release
		w.WriteHeader(http.StatusCreated)
	})

	var wg sync.WaitGroup
	codes := make(chan int, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- call(h, user, "ayni", `{}`).Code
		}()
	}
	// İlk istek işlemdeyken diğerleri 409 almalı.
	for len(codes) < 9 {
		runtime.Gosched()
	}
	close(release)
	wg.Wait()
	close(codes)
	created, inProgress := 0, 0
	for c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			inProgress++
		}
	}
	if calls.Load() != 1 || created != 1 || inProgress != 9 {
		t.Fatalf("calls=%d created=%d conflict=%d", calls.Load(), created, inProgress)
	}
}

func TestWithoutKeyOrUser(t *testing.T) {
	var calls atomic.Int64
	h, user := setup(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusCreated) })
	call(h, user, "", `{}`)
	call(h, user, "", `{}`)
	if calls.Load() != 2 {
		t.Fatal("anahtarsız istekler her seferinde işlenmeli")
	}
	if rec := call(h, "", "k", `{}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("kimliksiz anahtar 401: %d", rec.Code)
	}
	if rec := call(h, user, strings.Repeat("x", 201), `{}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("uzun anahtar 422: %d", rec.Code)
	}
}
