package order

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/inventory"
	"carsihub/internal/platform/dbtest"
	fx "carsihub/internal/platform/testfixture"
)

func newService(t *testing.T, policy inventory.Policy) (*Service, *pgxpool.Pool) {
	pool := dbtest.New(t)
	return NewService(pool, policy, 15*time.Minute), pool
}

func items(pairs ...any) []inventory.Item {
	var out []inventory.Item
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, inventory.Item{OfferID: pairs[i].(string), Quantity: pairs[i+1].(int)})
	}
	return out
}

func TestPlace_SplitsIntoSubOrdersPerSeller(t *testing.T) {
	s, pool := newService(t, inventory.PolicyAllowPartial)
	customer := fx.User(t, pool, "customer")
	sellerA, sellerB := fx.Seller(t, pool, ""), fx.Seller(t, pool, "")
	a1 := fx.Offer(t, pool, sellerA, 1050, 10)
	a2 := fx.Offer(t, pool, sellerA, 2000, 10)
	b1 := fx.Offer(t, pool, sellerB, 999, 10)

	o, err := s.Place(context.Background(), customer, items(a1, 2, b1, 3, a2, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	// 2×10,50 + 1×20,00 = 41,00 ; 3×9,99 = 29,97 ; toplam 70,97 TL
	if o.TotalKurus != 7097 || len(o.SubOrders) != 2 || len(o.Unavailable) != 0 {
		t.Fatalf("toplam=%d alt=%d", o.TotalKurus, len(o.SubOrders))
	}
	got, err := s.Get(context.Background(), o.ID)
	if err != nil {
		t.Fatal(err)
	}
	totals := map[string]int64{}
	for _, sub := range got.SubOrders {
		totals[sub.SellerID] = sub.TotalKurus
	}
	if totals[sellerA] != 4100 || totals[sellerB] != 2997 {
		t.Fatalf("alt sipariş toplamları yanlış: %v", totals)
	}
	if _, reserved := fx.Stock(t, pool, a1); reserved != 2 {
		t.Fatalf("rezervasyon siparişe bağlanmalı: %d", reserved)
	}
}

// Kısmi satış kuralı "reject": tek bir satıcının stoğu yetmezse sipariş hiç oluşmaz.
func TestPlace_RejectPolicyIsAllOrNothing(t *testing.T) {
	s, pool := newService(t, inventory.PolicyReject)
	customer := fx.User(t, pool, "customer")
	ok := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 10)
	scarce := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 1)

	_, err := s.Place(context.Background(), customer, items(ok, 2, scarce, 5), "")
	var stock *InsufficientStockError
	if !errors.As(err, &stock) || len(stock.Failures) != 1 || stock.Failures[0].OfferID != scarce {
		t.Fatalf("yetersiz stok hatası bekleniyordu: %v", err)
	}
	for _, offer := range []string{ok, scarce} {
		if _, reserved := fx.Stock(t, pool, offer); reserved != 0 {
			t.Fatalf("reject'te hiçbir stok tutulmamalı: %s reserved=%d", offer, reserved)
		}
	}
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM orders`).Scan(&n)
	if n != 0 {
		t.Fatal("sipariş oluşmamalı")
	}
}

// Kısmi satış kuralı "allow": stoğu olan satıcının kalemleri satılır, diğerleri raporlanır.
func TestPlace_AllowPolicySellsWhatIsAvailable(t *testing.T) {
	s, pool := newService(t, inventory.PolicyAllowPartial)
	customer := fx.User(t, pool, "customer")
	ok := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 10)
	scarce := fx.Offer(t, pool, fx.Seller(t, pool, ""), 500, 1)

	o, err := s.Place(context.Background(), customer, items(ok, 2, scarce, 5), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(o.SubOrders) != 1 || o.TotalKurus != 2000 {
		t.Fatalf("yalnızca stoğu olan satıcı siparişe girmeli: %+v", o)
	}
	if len(o.Unavailable) != 1 || o.Unavailable[0].OfferID != scarce || o.Unavailable[0].Available != 1 {
		t.Fatalf("satılamayan kalem raporlanmalı: %+v", o.Unavailable)
	}
	if _, reserved := fx.Stock(t, pool, scarce); reserved != 0 {
		t.Fatal("satılamayan kalem stok tutmamalı")
	}

	// Hiçbir kalem rezerve edilemiyorsa boş sipariş oluşmaz.
	if _, err := s.Place(context.Background(), customer, items(scarce, 5), ""); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("tamamen stoksuz sepet reddedilmeli: %v", err)
	}
}

// T-1 sipariş seviyesinde: aynı varyanta 1.000 eşzamanlı sipariş.
func TestPlace_ThousandConcurrentOrdersNeverOversell(t *testing.T) {
	s, pool := newService(t, inventory.PolicyReject)
	customer := fx.User(t, pool, "customer")
	const stock, requests = 100, 1000
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1999, stock)

	var placed, rejected atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Place(context.Background(), customer, items(offer, 1), "")
			switch {
			case err == nil:
				placed.Add(1)
			case errors.Is(err, ErrInsufficientStock):
				rejected.Add(1)
			default:
				t.Errorf("beklenmeyen hata: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	var orders, items int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM orders`).Scan(&orders)
	_ = pool.QueryRow(context.Background(), `SELECT coalesce(sum(quantity),0) FROM order_items`).Scan(&items)
	_, reserved := fx.Stock(t, pool, offer)
	t.Logf("sipariş isteği=%d oluşan=%d reddedilen=%d | sipariş tablosu=%d kalem toplamı=%d reserved=%d",
		requests, placed.Load(), rejected.Load(), orders, items, reserved)
	if placed.Load() != stock || orders != stock || items != stock || reserved != stock {
		t.Fatalf("tam %d sipariş bekleniyordu", stock)
	}
}

func TestCancel(t *testing.T) {
	s, pool := newService(t, inventory.PolicyAllowPartial)
	ctx := context.Background()
	customer, other := fx.User(t, pool, "customer"), fx.User(t, pool, "customer")
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 5)
	o, _ := s.Place(ctx, customer, items(offer, 3), "")

	if _, err := s.Cancel(ctx, other, o.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("başkasının siparişi bulunamadı sayılmalı: %v", err)
	}
	got, err := s.Cancel(ctx, customer, o.ID, "")
	if err != nil || got.Status != StatusCancelled || got.SubOrders[0].Status != string(StatusCancelled) {
		t.Fatalf("iptal: %+v %v", got, err)
	}
	if _, reserved := fx.Stock(t, pool, offer); reserved != 0 {
		t.Fatal("iptal stoğu iade etmeli")
	}
	if _, err := s.Cancel(ctx, customer, o.ID, ""); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("ikinci iptal reddedilmeli: %v", err)
	}
}

func TestPaymentOutcomes(t *testing.T) {
	s, pool := newService(t, inventory.PolicyAllowPartial)
	ctx := context.Background()
	customer := fx.User(t, pool, "customer")
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 10)

	paid, _ := s.Place(ctx, customer, items(offer, 4), "")
	failed, _ := s.Place(ctx, customer, items(offer, 3), "")

	// Çift ödeme bildirimi: ikinci çağrı etkisiz.
	for range 2 {
		if err := s.MarkPaid(ctx, paid.ID); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := s.FailPayment(ctx, failed.ID, "kart reddedildi"); err != nil {
			t.Fatal(err)
		}
	}
	onHand, reserved := fx.Stock(t, pool, offer)
	if onHand != 6 || reserved != 0 {
		t.Fatalf("ödenen 4 düşmeli, başarısız 3 iade edilmeli: on_hand=%d reserved=%d", onHand, reserved)
	}
	got, _ := s.Get(ctx, paid.ID)
	if got.Status != StatusPaid || got.SubOrders[0].Status != "awaiting_shipment" {
		t.Fatalf("ödenen sipariş durumu: %+v", got)
	}
	if err := s.MarkPaid(ctx, failed.ID); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("iptal edilmiş sipariş ödenmiş sayılamaz: %v", err)
	}
}

func TestExpireUnpaid_ReturnsStock(t *testing.T) {
	s, pool := newService(t, inventory.PolicyAllowPartial)
	ctx := context.Background()
	customer := fx.User(t, pool, "customer")
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 10)

	o, _ := s.Place(ctx, customer, items(offer, 4), "")
	s.now = func() time.Time { return time.Now().Add(time.Hour) }
	n, err := s.ExpireUnpaid(ctx, 100)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if _, reserved := fx.Stock(t, pool, offer); reserved != 0 {
		t.Fatal("süresi dolan sipariş stoğu iade etmeli")
	}
	got, _ := s.Get(ctx, o.ID)
	if got.Status != StatusExpired {
		t.Fatalf("durum %s", got.Status)
	}
	if n, _ := s.ExpireUnpaid(ctx, 100); n != 0 {
		t.Fatal("ikinci tur boş olmalı")
	}
}

func TestList_KeysetPagination(t *testing.T) {
	s, pool := newService(t, inventory.PolicyAllowPartial)
	ctx := context.Background()
	customer := fx.User(t, pool, "customer")
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 100)
	for range 5 {
		if _, err := s.Place(ctx, customer, items(offer, 1), ""); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.List(ctx, customer, nil, 3)
	if err != nil || len(first.Items) != 3 || first.NextCursor == "" {
		t.Fatalf("ilk sayfa: %d %q %v", len(first.Items), first.NextCursor, err)
	}
	if _, err := s.Get(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("olmayan sipariş: %v", err)
	}
}
