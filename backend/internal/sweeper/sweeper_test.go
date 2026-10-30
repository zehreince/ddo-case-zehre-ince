package sweeper

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"carsihub/internal/inventory"
	"carsihub/internal/order"
	"carsihub/internal/platform/dbtest"
	fx "carsihub/internal/platform/testfixture"
)

func TestSweepOnce_ReleasesExpiredCartsAndOrders(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	user := fx.User(t, pool, "customer")
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 10)

	// Süresi dolmuş sepet rezervasyonu (2) + süresi dolacak sipariş (3)
	tx, _ := pool.Begin(ctx)
	if _, err := inventory.Reserve(ctx, tx, user, inventory.Item{OfferID: offer, Quantity: 2}, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)
	orders := order.NewService(pool, inventory.PolicyAllowPartial, -time.Minute) // ödeme süresi hemen dolsun
	if _, err := orders.Place(ctx, user, []inventory.Item{{OfferID: offer, Quantity: 3}}, ""); err != nil {
		t.Fatal(err)
	}
	if _, reserved := fx.Stock(t, pool, offer); reserved != 5 {
		t.Fatalf("başlangıç reserved=%d", reserved)
	}

	s := New(pool, orders, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if s.Healthy() {
		t.Fatal("hiç tur atmadan sağlıklı sayılmamalı")
	}
	s.tick(ctx)
	if !s.Healthy() {
		t.Fatal("başarılı turdan sonra sağlıklı olmalı")
	}
	if _, reserved := fx.Stock(t, pool, offer); reserved != 0 {
		t.Fatalf("tüm stok iade edilmeli, reserved=%d", reserved)
	}
	carts, ordersN, err := s.SweepOnce(ctx)
	if err != nil || carts != 0 || ordersN != 0 {
		t.Fatalf("ikinci tur boş olmalı: %d %d %v", carts, ordersN, err)
	}

	s.now = func() time.Time { return time.Now().Add(time.Minute) }
	if s.Healthy() {
		t.Fatal("uzun süre tur atmayan süpürücü sağlıksız sayılmalı")
	}
	if len(s.Collectors()) != 2 {
		t.Fatal("iki metrik bekleniyordu")
	}
}

func TestRun_StopsWithContext(t *testing.T) {
	pool := dbtest.New(t)
	s := New(pool, order.NewService(pool, inventory.PolicyAllowPartial, time.Minute), 10*time.Millisecond,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run bağlam bitince dönmeli")
	}
	if !s.Healthy() {
		t.Fatal("en az bir tur atmış olmalı")
	}
}
