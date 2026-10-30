package settlement

import (
	"context"
	"errors"
	"testing"
	"time"

	"carsihub/internal/inventory"
	"carsihub/internal/ledger"
	"carsihub/internal/order"
	"carsihub/internal/outbox"
	"carsihub/internal/platform/dbtest"
	fx "carsihub/internal/platform/testfixture"
)

// Ödeme → teslim: hakediş kalemi kategoriye ve sipariş anına göre komisyonla
// hesaplanır, deftere yazılır; olay tekrar gelse de bir kez.
func TestCaptureAndAccrue(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	s := NewService(pool, 14*24*time.Hour, 2990)
	seller := fx.Seller(t, pool, "")
	offer := fx.Offer(t, pool, seller, 10000, 10) // 100 TL
	cat := fx.OfferCategory(t, pool, offer)
	if _, err := pool.Exec(ctx, `
		INSERT INTO commission_rates (category_id, rate_bps, valid_from, valid_to) VALUES
			(NULL, 1500, '2020-01-01', NULL),
			($1, 1000, '2020-01-01', now() + interval '1 hour'),   -- sipariş anında geçerli: %10
			($1, 3000, now() + interval '1 hour', NULL)`, cat); err != nil { // gelecekte: %30, uygulanmamalı
		t.Fatal(err)
	}
	// Aynı kategoride çakışan aralık veritabanında reddedilir.
	if _, err := pool.Exec(ctx, `INSERT INTO commission_rates (category_id, rate_bps, valid_from) VALUES ($1, 500, '2021-01-01')`, cat); err == nil {
		t.Fatal("çakışan komisyon aralığı reddedilmeli")
	}

	orders := order.NewService(pool, inventory.PolicyAllowPartial, time.Hour)
	fx.Coupon(t, pool, fx.CouponSpec{Code: "PAZAR20", Scope: "marketplace", Kind: "fixed", Value: 2000})
	o, err := orders.Place(ctx, fx.User(t, pool, "customer"), []inventory.Item{{OfferID: offer, Quantity: 2}}, "", order.WithCoupons([]string{"PAZAR20"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := orders.MarkPaid(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	var paid outbox.Event
	_ = pool.QueryRow(ctx, `SELECT id, payload, created_at FROM outbox_events WHERE event_type = 'order.paid'`).Scan(&paid.ID, &paid.Payload, &paid.CreatedAt)
	delivered := outbox.Event{ID: paid.ID, Payload: []byte(`{"sub_order_id":"` + o.SubOrders[0].ID + `","delivered_at":"2030-01-01T00:00:00Z"}`)}
	// Teslim olayı için ayrı bir outbox kaydı (Once kaydı olay kimliğine bağlı).
	tx, _ := pool.Begin(ctx)
	_ = outbox.Write(ctx, tx, "shipment.delivered", "sub_order", o.SubOrders[0].ID, map[string]string{"sub_order_id": o.SubOrders[0].ID, "delivered_at": "2030-01-01T00:00:00Z"})
	_ = tx.Commit(ctx)
	_ = pool.QueryRow(ctx, `SELECT id FROM outbox_events WHERE event_type = 'shipment.delivered'`).Scan(&delivered.ID)

	for range 2 {
		if err := s.CaptureConsumer().Handle(ctx, paid); err != nil {
			t.Fatal(err)
		}
		if err := s.AccrueConsumer().Handle(ctx, delivered); err != nil {
			t.Fatal(err)
		}
	}

	var gross, commission, shipping, net int64
	var due time.Time
	if err := pool.QueryRow(ctx, `SELECT gross_kurus, commission_kurus, shipping_fee_kurus, net_kurus, due_at FROM settlement_items`).
		Scan(&gross, &commission, &shipping, &net, &due); err != nil {
		t.Fatal(err)
	}
	// Brüt 200 TL (pazaryeri kuponunu pazaryeri karşılar), komisyon %10 = 20, kargo 29,90 → net 150,10
	if gross != 20000 || commission != 2000 || shipping != 2990 || net != 15010 {
		t.Fatalf("hakediş: brüt=%d komisyon=%d kargo=%d net=%d", gross, commission, shipping, net)
	}
	if !due.Equal(time.Date(2030, 1, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("vade teslim + 14 gün olmalı: %v", due)
	}

	if b, _ := ledger.Balance(ctx, pool, ledger.SellerPayable(seller)); b != 15010 {
		t.Fatalf("defterde satıcı alacağı: %d", b)
	}
	if b, _ := ledger.Balance(ctx, pool, ledger.Cash); b != 18000 {
		t.Fatalf("kasa (müşterinin ödediği 180 TL): %d", b)
	}
	if b, _ := ledger.Balance(ctx, pool, ledger.CustomerAdvances); b != 0 {
		t.Fatalf("teslimden sonra müşteri avansı kapanmalı: %d", b)
	}
	if b, _ := ledger.Balance(ctx, pool, ledger.MarketplaceDiscount); b != 2000 {
		t.Fatalf("pazaryeri indirim gideri: %d", b)
	}
	var txs int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions`).Scan(&txs)
	if txs != 2 {
		t.Fatalf("olaylar tekrar gelse de 2 işlem olmalı: %d", txs)
	}
}

func TestRateAt_NoRate(t *testing.T) {
	pool := dbtest.New(t)
	if _, err := RateAt(context.Background(), pool, 1, time.Now()); !errors.Is(err, ErrNoRate) {
		t.Fatalf("oran yoksa hata: %v", err)
	}
}
