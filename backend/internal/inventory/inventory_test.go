package inventory_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/inventory"
	"carsihub/internal/platform/dbtest"
	fx "carsihub/internal/platform/testfixture"
)

var future = time.Now().Add(time.Hour)

func reserve(t *testing.T, pool *pgxpool.Pool, userID, offerID string, qty int, exp time.Time) (*inventory.Reservation, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	r, err := inventory.Reserve(ctx, tx, userID, inventory.Item{OfferID: offerID, Quantity: qty}, exp)
	if err != nil {
		return nil, err
	}
	return r, tx.Commit(ctx)
}

// T-1 ana kanıtı: aynı teklife 1.000 eşzamanlı rezervasyon, stoktan fazlası satılamaz.
func TestReserve_ThousandConcurrentRequestsNeverOversell(t *testing.T) {
	pool := dbtest.New(t)
	user := fx.User(t, pool, "customer")
	const stock, requests = 50, 1000
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, stock)

	var ok, insufficient, other atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // hepsi aynı anda başlasın
			_, err := reserve(t, pool, user, offer, 1, future)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, inventory.ErrInsufficientStock):
				insufficient.Add(1)
			default:
				other.Add(1)
				t.Errorf("beklenmeyen hata: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	onHand, reserved := fx.Stock(t, pool, offer)
	t.Logf("istek=%d başarılı=%d stok-yetersiz=%d | on_hand=%d reserved=%d", requests, ok.Load(), insufficient.Load(), onHand, reserved)
	if ok.Load() != stock || insufficient.Load() != requests-stock || other.Load() != 0 {
		t.Fatalf("tam %d başarılı bekleniyordu", stock)
	}
	if reserved != stock || fx.ActiveReserved(t, pool, offer) != stock {
		t.Fatalf("offers.stock_reserved=%d ile aktif rezervasyon toplamı %d olmalı", reserved, stock)
	}
}

// Farklı adetlerle karışık eşzamanlı istekler: toplam hiçbir zaman stoğu aşmaz
// ve stok yetersizken reddedilen her istek gerçekten sığmayan istektir.
func TestReserve_MixedQuantitiesStayWithinStock(t *testing.T) {
	pool := dbtest.New(t)
	user := fx.User(t, pool, "customer")
	const stock = 97
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, stock)

	var reservedSum atomic.Int64
	var wg sync.WaitGroup
	for i := range 300 {
		wg.Add(1)
		go func(q int) {
			defer wg.Done()
			if _, err := reserve(t, pool, user, offer, q, future); err == nil {
				reservedSum.Add(int64(q))
			} else if !errors.Is(err, inventory.ErrInsufficientStock) {
				t.Errorf("beklenmeyen hata: %v", err)
			}
		}(1 + i%5)
	}
	wg.Wait()

	_, reserved := fx.Stock(t, pool, offer)
	if int64(reserved) != reservedSum.Load() || reserved > stock {
		t.Fatalf("reserved=%d, başarılı istek toplamı=%d, stok=%d", reserved, reservedSum.Load(), stock)
	}
	// Kalan stok en küçük isteği (1) bile karşılayamamalı; yoksa gereksiz ret var demektir.
	if stock-reserved >= 1 {
		if _, err := reserve(t, pool, user, offer, stock-reserved, future); err != nil {
			t.Fatalf("kalan %d birim alınabilmeliydi: %v", stock-reserved, err)
		}
	}
}

func TestReserve_UnknownOrInactiveOffer(t *testing.T) {
	pool := dbtest.New(t)
	user := fx.User(t, pool, "customer")
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 5)
	if _, err := pool.Exec(context.Background(), `UPDATE offers SET active = false WHERE id = $1::uuid`, offer); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{offer, "00000000-0000-0000-0000-000000000000"} {
		if _, err := reserve(t, pool, user, id, 1, future); !errors.Is(err, inventory.ErrOfferNotFound) {
			t.Errorf("%s: ErrOfferNotFound bekleniyordu, gelen %v", id, err)
		}
	}
}

// Uygulama katmanı atlanırsa bile veritabanı eldekinden fazla rezervasyona izin vermez.
func TestOffersCheckConstraintIsLastLineOfDefence(t *testing.T) {
	pool := dbtest.New(t)
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 3)
	_, err := pool.Exec(context.Background(), `UPDATE offers SET stock_reserved = 4 WHERE id = $1::uuid`, offer)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.ConstraintName != "offers_reserved_le_on_hand" {
		t.Fatalf("CHECK kısıtı devreye girmeli: %v", err)
	}
}

func TestReleaseAndCommit(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	user := fx.User(t, pool, "customer")
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 10)

	a, _ := reserve(t, pool, user, offer, 3, future)
	b, _ := reserve(t, pool, user, offer, 4, future)

	// Ödeme başarısız → stok iade, tekrar bırakmak etkisiz.
	for range 2 {
		tx, _ := pool.Begin(ctx)
		if _, err := inventory.Release(ctx, tx, []string{a.ID}, "released"); err != nil {
			t.Fatal(err)
		}
		_ = tx.Commit(ctx)
	}
	if onHand, reserved := fx.Stock(t, pool, offer); onHand != 10 || reserved != 4 {
		t.Fatalf("bırakma sonrası on_hand=%d reserved=%d", onHand, reserved)
	}

	// Ödeme başarılı → stok eldekinden düşer.
	tx, _ := pool.Begin(ctx)
	if err := inventory.Commit(ctx, tx, []string{b.ID}); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)
	if onHand, reserved := fx.Stock(t, pool, offer); onHand != 6 || reserved != 0 {
		t.Fatalf("kesinleştirme sonrası on_hand=%d reserved=%d", onHand, reserved)
	}

	// Bırakılmış rezervasyon kesinleştirilemez.
	tx, _ = pool.Begin(ctx)
	defer func() { _ = tx.Rollback(ctx) }()
	if err := inventory.Commit(ctx, tx, []string{a.ID}); !errors.Is(err, inventory.ErrReservationNotActive) {
		t.Fatalf("ErrReservationNotActive bekleniyordu: %v", err)
	}
	if _, err := inventory.Release(ctx, tx, []string{a.ID}, "committed"); err == nil {
		t.Fatal("geçersiz bırakma durumu reddedilmeli")
	}
}

func TestReserveBatch_CollectsAllFailures(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	user := fx.User(t, pool, "customer")
	plenty := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 10)
	scarce := fx.Offer(t, pool, fx.Seller(t, pool, ""), 2000, 1)

	tx, _ := pool.Begin(ctx)
	defer func() { _ = tx.Rollback(ctx) }()
	reserved, failed, err := inventory.ReserveBatch(ctx, tx, user, []inventory.Item{
		{OfferID: plenty, Quantity: 2},
		{OfferID: scarce, Quantity: 3},
		{OfferID: plenty, Quantity: 1}, // aynı teklif birleştirilir → 3
		{OfferID: "00000000-0000-0000-0000-000000000000", Quantity: 1},
	}, future)
	if err != nil {
		t.Fatal(err)
	}
	if len(reserved) != 1 || reserved[0].OfferID != plenty || reserved[0].Quantity != 3 {
		t.Fatalf("reserved=%+v", reserved)
	}
	if len(failed) != 2 {
		t.Fatalf("failed=%+v", failed)
	}
	for _, f := range failed {
		switch f.OfferID {
		case scarce:
			if f.Reason != "insufficient_stock" || f.Requested != 3 || f.Available != 1 {
				t.Errorf("yetersiz stok kaydı yanlış: %+v", f)
			}
		default:
			if f.Reason != "offer_not_found" {
				t.Errorf("bulunamayan teklif kaydı yanlış: %+v", f)
			}
		}
	}
	// Başarısız kalemin savepoint'i geri alındı; başarılı olan duruyor.
	var scarceReserved int
	_ = tx.QueryRow(ctx, `SELECT stock_reserved FROM offers WHERE id = $1::uuid`, scarce).Scan(&scarceReserved)
	if scarceReserved != 0 {
		t.Fatalf("başarısız kalem stok tutmamalı: %d", scarceReserved)
	}
}

// İki işlem aynı iki teklifi ters sırada istese de kilit sırası sabit olduğu için deadlock olmaz.
func TestReserveBatch_OppositeOrderDoesNotDeadlock(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	user := fx.User(t, pool, "customer")
	a := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 1000)
	b := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 1000)

	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items := []inventory.Item{{OfferID: a, Quantity: 1}, {OfferID: b, Quantity: 1}}
			if i%2 == 1 {
				items[0], items[1] = items[1], items[0]
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, _, err := inventory.ReserveBatch(ctx, tx, user, items, future); err != nil {
				errs <- err
				return
			}
			errs <- tx.Commit(ctx)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("deadlock ya da hata: %v", err)
		}
	}
	if _, r := fx.Stock(t, pool, a); r != 100 {
		t.Fatalf("a reserved=%d", r)
	}
}

func TestExpireCartReservations(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	user := fx.User(t, pool, "customer")
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 10)

	expired, _ := reserve(t, pool, user, offer, 2, time.Now().Add(-time.Second))
	_, _ = reserve(t, pool, user, offer, 3, future)
	if _, err := pool.Exec(ctx, `INSERT INTO cart_items (user_id, offer_id, quantity, reservation_id) VALUES ($1::uuid, $2::uuid, 2, $3::uuid)`,
		user, offer, expired.ID); err != nil {
		t.Fatal(err)
	}

	tx, _ := pool.Begin(ctx)
	n, err := inventory.ExpireCartReservations(ctx, tx, time.Now(), 100)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	_ = tx.Commit(ctx)

	if _, reserved := fx.Stock(t, pool, offer); reserved != 3 {
		t.Fatalf("süresi dolan 2 birim iade edilmeli, reserved=%d", reserved)
	}
	var res *string
	_ = pool.QueryRow(ctx, `SELECT reservation_id::text FROM cart_items WHERE user_id = $1::uuid`, user).Scan(&res)
	if res != nil {
		t.Fatal("sepet kalemi kalmalı ama rezervasyonu düşmeli")
	}
}

func TestMergeItemsAndPolicy(t *testing.T) {
	got := inventory.MergeItems([]inventory.Item{{OfferID: "b", Quantity: 1}, {OfferID: "a", Quantity: 2}, {OfferID: "b", Quantity: 4}})
	if len(got) != 2 || got[0] != (inventory.Item{OfferID: "a", Quantity: 2}) || got[1] != (inventory.Item{OfferID: "b", Quantity: 5}) {
		t.Fatalf("got=%+v", got)
	}
	for _, s := range []string{"allow", "reject"} {
		if _, err := inventory.ParsePolicy(s); err != nil {
			t.Errorf("%s geçerli olmalı", s)
		}
	}
	if _, err := inventory.ParsePolicy("bazen"); err == nil {
		t.Error("geçersiz politika reddedilmeli")
	}
}
