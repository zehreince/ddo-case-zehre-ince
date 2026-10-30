package order

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/inventory"
	fx "carsihub/internal/platform/testfixture"
)

func intp(n int) *int { return &n }

// assertConsistent sipariş, alt sipariş ve satır tutarlarının kuruşu kuruşuna tuttuğunu doğrular.
func assertConsistent(t *testing.T, pool *pgxpool.Pool, orderID string) {
	t.Helper()
	var orderTotal, orderDisc, subTotals, subDisc, itemPaid, itemDisc, couponDisc int64
	err := pool.QueryRow(context.Background(), `
		SELECT o.total_kurus, o.discount_kurus,
		       (SELECT sum(total_kurus) FROM sub_orders WHERE order_id = o.id),
		       (SELECT sum(discount_kurus) FROM sub_orders WHERE order_id = o.id),
		       (SELECT sum(paid_kurus) FROM order_items WHERE order_id = o.id),
		       (SELECT sum(marketplace_discount_kurus + seller_discount_kurus) FROM order_items WHERE order_id = o.id),
		       (SELECT COALESCE(sum(discount_kurus), 0) FROM order_coupons WHERE order_id = o.id)
		FROM orders o WHERE o.id = $1::uuid`, orderID).
		Scan(&orderTotal, &orderDisc, &subTotals, &subDisc, &itemPaid, &itemDisc, &couponDisc)
	if err != nil {
		t.Fatal(err)
	}
	if orderTotal != subTotals || orderTotal != itemPaid || orderDisc != subDisc || orderDisc != itemDisc || orderDisc != couponDisc {
		t.Fatalf("tutarlar tutarsız: sipariş=%d/%d alt=%d/%d satır=%d/%d kupon=%d",
			orderTotal, orderDisc, subTotals, subDisc, itemPaid, itemDisc, couponDisc)
	}
}

func TestPlace_WithCouponsSplitsDiscountBySeller(t *testing.T) {
	s, pool := newService(t, inventory.PolicyAllowPartial)
	ctx := context.Background()
	customer := fx.User(t, pool, "customer")
	sellerA, sellerB := fx.Seller(t, pool, ""), fx.Seller(t, pool, "")
	a := fx.Offer(t, pool, sellerA, 10000, 10) // 100 TL
	b := fx.Offer(t, pool, sellerB, 5001, 10)  // 50,01 TL
	fx.Coupon(t, pool, fx.CouponSpec{Code: "PAZAR30", Scope: "marketplace", Kind: "fixed", Value: 3000})
	fx.Coupon(t, pool, fx.CouponSpec{Code: "SATICIA10", Scope: "seller", SellerID: sellerA, Kind: "percent", Value: 1000})

	o, err := s.Place(ctx, customer, items(a, 2, b, 1), "", WithCoupons([]string{"pazar30", "SATICIA10"}))
	if err != nil {
		t.Fatal(err)
	}
	// A: 200 - %10 = 180; pazaryeri 30 TL, 180 ve 50,01 üzerinden orantılı.
	if o.SubtotalKurus != 25001 || o.DiscountKurus != 2000+3000 || o.TotalKurus != 20001 || len(o.AppliedCoupons) != 2 {
		t.Fatalf("sipariş: %+v", o)
	}
	got, _ := s.Get(ctx, o.ID)
	for _, sub := range got.SubOrders {
		switch sub.SellerID {
		case sellerA:
			if sub.SellerDiscountKurus != 2000 || sub.MarketplaceDiscountKurus == 0 {
				t.Fatalf("A alt siparişi: %+v", sub)
			}
		case sellerB:
			if sub.SellerDiscountKurus != 0 || sub.MarketplaceDiscountKurus == 0 {
				t.Fatalf("B alt siparişi: %+v", sub)
			}
		}
	}
	if len(got.AppliedCoupons) != 2 {
		t.Fatalf("kupon anlık görüntüleri: %+v", got.AppliedCoupons)
	}
	assertConsistent(t, pool, o.ID)
	var paymentAmount int64
	_ = pool.QueryRow(ctx, `SELECT amount_kurus FROM payments WHERE order_id = $1::uuid`, o.ID).Scan(&paymentAmount)
	if paymentAmount != o.TotalKurus {
		t.Fatalf("ödeme tutarı indirimli toplam olmalı: %d", paymentAmount)
	}
}

func TestPlace_RejectsUnknownExpiredAndIneligibleCoupons(t *testing.T) {
	s, pool := newService(t, inventory.PolicyAllowPartial)
	ctx := context.Background()
	customer := fx.User(t, pool, "customer")
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 10000, 10)
	fx.Coupon(t, pool, fx.CouponSpec{Code: "BITTI", Scope: "marketplace", Kind: "fixed", Value: 100})
	if _, err := pool.Exec(ctx, `UPDATE coupons SET ends_at = now() - interval '1 second', starts_at = now() - interval '1 day' WHERE code = 'BITTI'`); err != nil {
		t.Fatal(err)
	}
	fx.Coupon(t, pool, fx.CouponSpec{Code: "MIN500", Scope: "marketplace", Kind: "fixed", Value: 100, MinBasket: 50000})

	o, err := s.Place(ctx, customer, items(offer, 1), "", WithCoupons([]string{"YOK", "BITTI", "MIN500"}))
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, r := range o.RejectedCoupons {
		reasons[r.Code] = r.Reason
	}
	if reasons["YOK"] != "not_found" || reasons["BITTI"] != "expired" || reasons["MIN500"] != "min_basket" || o.DiscountKurus != 0 {
		t.Fatalf("ret nedenleri: %v", reasons)
	}
}

// Kısmi satışta satılamayan kalem düşünce minimum sepet bozulursa kupon reddedilir.
func TestPlace_PartialFulfillmentCanDropCoupon(t *testing.T) {
	s, pool := newService(t, inventory.PolicyAllowPartial)
	customer := fx.User(t, pool, "customer")
	ok := fx.Offer(t, pool, fx.Seller(t, pool, ""), 20000, 10)
	scarce := fx.Offer(t, pool, fx.Seller(t, pool, ""), 20000, 0)
	fx.Coupon(t, pool, fx.CouponSpec{Code: "MIN300", Scope: "marketplace", Kind: "fixed", Value: 5000, MinBasket: 30000})

	o, err := s.Place(context.Background(), customer, items(ok, 1, scarce, 1), "", WithCoupons([]string{"MIN300"}))
	if err != nil {
		t.Fatal(err)
	}
	if o.DiscountKurus != 0 || len(o.RejectedCoupons) != 1 || o.RejectedCoupons[0].Reason != "min_basket" || len(o.Unavailable) != 1 {
		t.Fatalf("kupon düşmeliydi: %+v", o)
	}
}

// T-3 kullanım limiti: 10 hakkı olan kupona 60 eşzamanlı sipariş → tam 10 indirimli.
func TestPlace_CouponUsageLimitUnderConcurrency(t *testing.T) {
	s, pool := newService(t, inventory.PolicyAllowPartial)
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 10000, 1000)
	fx.Coupon(t, pool, fx.CouponSpec{Code: "SINIRLI", Scope: "marketplace", Kind: "fixed", Value: 1000, MaxRedemptions: intp(10)})

	const n = 60
	customers := make([]string, n)
	for i := range customers {
		customers[i] = fx.User(t, pool, "customer")
	}
	var discounted, limited atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, c := range customers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			o, err := s.Place(context.Background(), c, items(offer, 1), "", WithCoupons([]string{"SINIRLI"}))
			switch {
			case err != nil:
				t.Error(err)
			case o.DiscountKurus == 1000:
				discounted.Add(1)
			case len(o.RejectedCoupons) == 1 && o.RejectedCoupons[0].Reason == "usage_limit":
				limited.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	var redeemed, active int
	_ = pool.QueryRow(context.Background(), `
		SELECT redeemed_count, (SELECT count(*) FROM coupon_redemptions WHERE status = 'active') FROM coupons WHERE code = 'SINIRLI'`).
		Scan(&redeemed, &active)
	t.Logf("sipariş=%d indirimli=%d limit-dolu=%d | redeemed_count=%d aktif kullanım=%d", n, discounted.Load(), limited.Load(), redeemed, active)
	if discounted.Load() != 10 || limited.Load() != n-10 || redeemed != 10 || active != 10 {
		t.Fatal("kupon limiti aşılmamalı")
	}
}

func TestPlace_PerUserLimitAndReleaseOnCancel(t *testing.T) {
	s, pool := newService(t, inventory.PolicyAllowPartial)
	ctx := context.Background()
	customer := fx.User(t, pool, "customer")
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 10000, 100)
	fx.Coupon(t, pool, fx.CouponSpec{Code: "TEKSEFER", Scope: "marketplace", Kind: "fixed", Value: 500, PerUserLimit: intp(1)})

	first, err := s.Place(ctx, customer, items(offer, 1), "", WithCoupons([]string{"TEKSEFER"}))
	if err != nil || first.DiscountKurus != 500 {
		t.Fatalf("ilk kullanım: %+v %v", first, err)
	}
	second, err := s.Place(ctx, customer, items(offer, 1), "", WithCoupons([]string{"TEKSEFER"}))
	if err != nil || second.DiscountKurus != 0 || second.RejectedCoupons[0].Reason != "user_limit" {
		t.Fatalf("ikinci kullanım reddedilmeli: %+v %v", second, err)
	}
	// İptal kullanım hakkını geri verir.
	if _, err := s.Cancel(ctx, customer, first.ID, ""); err != nil {
		t.Fatal(err)
	}
	third, err := s.Place(ctx, customer, items(offer, 1), "", WithCoupons([]string{"TEKSEFER"}))
	if err != nil || third.DiscountKurus != 500 {
		t.Fatalf("iptal sonrası kupon tekrar kullanılabilmeli: %+v %v", third, err)
	}
	var redeemed int
	_ = pool.QueryRow(ctx, `SELECT redeemed_count FROM coupons WHERE code = 'TEKSEFER'`).Scan(&redeemed)
	if redeemed != 1 {
		t.Fatalf("redeemed_count: %d", redeemed)
	}
}

// Kupon tutarın tamamını karşılarsa tahsilat yok: sipariş doğrudan ödenmiş olur.
func TestPlace_FullyDiscountedOrderIsPaid(t *testing.T) {
	s, pool := newService(t, inventory.PolicyAllowPartial)
	customer := fx.User(t, pool, "customer")
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 5000, 10)
	fx.Coupon(t, pool, fx.CouponSpec{Code: "BEDAVA", Scope: "marketplace", Kind: "fixed", Value: 99999})

	o, err := s.Place(context.Background(), customer, items(offer, 1), "", WithCoupons([]string{"BEDAVA"}))
	if err != nil || o.Status != StatusPaid || o.TotalKurus != 0 {
		t.Fatalf("%+v %v", o, err)
	}
	var payments, paidEvents int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM payments WHERE order_id = $1::uuid`, o.ID).Scan(&payments)
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events WHERE event_type = 'order.paid' AND aggregate_id = $1`, o.ID).Scan(&paidEvents)
	if payments != 0 || paidEvents != 1 {
		t.Fatalf("ödeme kaydı olmamalı, order.paid yazılmalı: payments=%d events=%d", payments, paidEvents)
	}
	if onHand, reserved := fx.Stock(t, pool, offer); onHand != 9 || reserved != 0 {
		t.Fatalf("stok kesinleşmeli: %d/%d", onHand, reserved)
	}
}

func TestRefundQuote(t *testing.T) {
	s, pool := newService(t, inventory.PolicyAllowPartial)
	ctx := context.Background()
	customer := fx.User(t, pool, "customer")
	a := fx.Offer(t, pool, fx.Seller(t, pool, ""), 20000, 10)
	b := fx.Offer(t, pool, fx.Seller(t, pool, ""), 10000, 10)
	fx.Coupon(t, pool, fx.CouponSpec{Code: "MIN250", Scope: "marketplace", Kind: "fixed", Value: 5000, MinBasket: 25000})

	o, err := s.Place(ctx, customer, items(a, 1, b, 1), "", WithCoupons([]string{"MIN250"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefundQuote(ctx, o.ID, nil); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("ödenmemiş sipariş için iade hesaplanmaz: %v", err)
	}
	if err := s.MarkPaid(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, o.ID)
	var itemB string
	for _, sub := range got.SubOrders {
		for _, l := range sub.Items {
			if l.OfferID == b {
				itemB = l.ID
			}
		}
	}
	rq, err := s.RefundQuote(ctx, o.ID, []ReturnItem{{OrderItemID: itemB, Quantity: 1}})
	if err != nil {
		t.Fatal(err)
	}
	// 300 - 50 = 250 ödendi; b iade → 200 < 250, kupon düşer → iade 50 TL, kesinti 33,33 TL.
	if rq.RefundKurus != 5000 || rq.ClawbackKurus != 3333 || len(rq.LostCoupons) != 1 {
		t.Fatalf("iade: %+v", rq)
	}
	for _, bad := range [][]ReturnItem{
		{{OrderItemID: "00000000-0000-0000-0000-000000000000", Quantity: 1}},
		{{OrderItemID: itemB, Quantity: 2}},
		{},
	} {
		if _, err := s.RefundQuote(ctx, o.ID, bad); !errors.Is(err, ErrInvalidReturn) {
			t.Errorf("geçersiz iade reddedilmeli: %v", err)
		}
	}
	if _, err := s.RefundQuote(ctx, "00000000-0000-0000-0000-000000000000", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("olmayan sipariş: %v", err)
	}
}
