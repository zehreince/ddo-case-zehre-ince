package simulator_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"carsihub/internal/payment"
	"carsihub/internal/platform/webhook"
	"carsihub/internal/shipping"
	"carsihub/internal/simulator"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

type received struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (r *received) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		if err := webhook.Verify(secret, req.Header, body, time.Now()); err != nil {
			t.Errorf("simülatörün callback'i imzalı olmalı: %v", err)
		}
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		r.mu.Lock()
		r.bodies = append(r.bodies, m)
		r.mu.Unlock()
	}
}

func (r *received) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func start(t *testing.T) (*simulator.Server, string, *received) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sim := simulator.New(ctx, simulator.Config{
		Seed: 42, Secret: secret, DeclineRate: 0.1, LostCallbackRate: 0.1, DupCallbackRate: 0.1,
		PaymentLatency: 10 * time.Millisecond, ShipmentStep: 10 * time.Millisecond,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(sim.Handler())
	t.Cleanup(srv.Close)
	cb := &received{}
	cbSrv := httptest.NewServer(cb.handler(t))
	t.Cleanup(cbSrv.Close)
	return sim, srv.URL, cb
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("koşul sağlanmadı")
}

func TestDecideMode_IsDeterministicAndRespectsRates(t *testing.T) {
	sim, _, _ := start(t)
	magic := map[int64]simulator.Mode{
		10001: simulator.ModeDecline, 10002: simulator.ModeLostCallback, 10003: simulator.ModeDupCallback,
		10004: simulator.ModeHang, 10005: simulator.ModeSuccess,
	}
	for amount, want := range magic {
		if got := sim.DecideMode("herhangi", amount); got != want {
			t.Errorf("%d → %s, beklenen %s", amount, got, want)
		}
	}
	counts := map[simulator.Mode]int{}
	for i := range 10000 {
		id := "p" + string(rune('a'+i%26)) + time.Duration(i).String()
		m := sim.DecideMode(id, 10000)
		if m != sim.DecideMode(id, 10000) {
			t.Fatal("aynı tohum + kimlik aynı sonucu vermeli")
		}
		counts[m]++
	}
	for _, m := range []simulator.Mode{simulator.ModeDecline, simulator.ModeLostCallback, simulator.ModeDupCallback} {
		if counts[m] < 800 || counts[m] > 1200 {
			t.Errorf("%s oranı ~%%10 olmalı: %d/10000", m, counts[m])
		}
	}
}

func TestPayments_EndToEndWithHTTPProvider(t *testing.T) {
	_, base, _ := start(t)
	cb := &received{}
	cbSrv := httptest.NewServer(cb.handler(t))
	defer cbSrv.Close()
	p := payment.NewHTTPProvider(base, secret, time.Second)
	ctx := context.Background()

	cases := []struct {
		id        string
		amount    int64
		callbacks int
		status    string
	}{
		{"basarili", 10005, 1, payment.ProviderSucceeded},
		{"ret", 10001, 1, payment.ProviderFailed},
		{"kayip", 10002, 0, payment.ProviderSucceeded},
		{"cift", 10003, 2, payment.ProviderSucceeded},
		{"askida", 10004, 0, payment.ProviderPending},
	}
	for _, tc := range cases {
		if err := p.Create(ctx, tc.id, tc.amount, cbSrv.URL); err != nil {
			t.Fatal(err)
		}
		if err := p.Create(ctx, tc.id, tc.amount, cbSrv.URL); err != nil { // idempotent
			t.Fatal(err)
		}
	}
	eventually(t, func() bool { return cb.count() == 4 })
	time.Sleep(50 * time.Millisecond) // fazladan callback gelmediğinden emin ol
	if cb.count() != 4 {
		t.Fatalf("toplam callback: %d", cb.count())
	}
	for _, tc := range cases {
		st, _, err := p.Status(ctx, tc.id)
		if err != nil || st != tc.status {
			t.Errorf("%s: %s %v", tc.id, st, err)
		}
	}
	if _, _, err := p.Status(ctx, "yok"); !errors.Is(err, payment.ErrProviderNotFound) {
		t.Fatalf("bilinmeyen ödeme: %v", err)
	}
	if err := p.Refund(ctx, "basarili"); err != nil {
		t.Fatal(err)
	}
	if err := p.Refund(ctx, "ret"); err == nil {
		t.Fatal("başarısız ödeme iade edilemez")
	}
}

func TestRejectsUnsignedRequests(t *testing.T) {
	_, base, _ := start(t)
	bad := payment.NewHTTPProvider(base, []byte("baska-bir-anahtar-baska-bir-anahtar"), time.Second)
	if err := bad.Create(context.Background(), "x", 100, ""); err == nil {
		t.Fatal("yanlış anahtarla imzalı istek reddedilmeli")
	}
	resp, err := http.Get(base + "/stats")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("stats: %v", err)
	}
	_ = resp.Body.Close()
}

func TestShipments_EmitOrderedEventsWithDuplicateDelivery(t *testing.T) {
	_, base, cb := start(t)
	cbSrv := httptest.NewServer(cb.handler(t))
	defer cbSrv.Close()
	c := shipping.NewHTTPCarrier(base, secret, time.Second)

	tr1, err := c.Create(context.Background(), "alt-siparis-1", cbSrv.URL)
	if err != nil || tr1 == "" {
		t.Fatal(err)
	}
	tr2, _ := c.Create(context.Background(), "alt-siparis-1", cbSrv.URL)
	if tr1 != tr2 {
		t.Fatal("aynı gönderi aynı takip numarasını almalı")
	}
	eventually(t, func() bool { return cb.count() == 4 })
	var statuses []string
	for _, b := range cb.bodies {
		statuses = append(statuses, b["status"].(string))
	}
	want := []string{"picked_up", "in_transit", "delivered", "delivered"}
	for i := range want {
		if statuses[i] != want[i] {
			t.Fatalf("olay sırası: %v", statuses)
		}
	}
}
