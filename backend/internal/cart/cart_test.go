package cart

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/inventory"
	"carsihub/internal/order"
	"carsihub/internal/platform/dbtest"
	fx "carsihub/internal/platform/testfixture"
)

func newService(t *testing.T, policy inventory.Policy) (*Service, *pgxpool.Pool) {
	pool := dbtest.New(t)
	return NewService(pool, order.NewService(pool, policy, 15*time.Minute), 10*time.Minute), pool
}

func TestSetItem_ReservesAndReplacesReservation(t *testing.T) {
	s, pool := newService(t, inventory.PolicyAllowPartial)
	ctx := context.Background()
	user := fx.User(t, pool, "customer")
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1250, 5)

	c, err := s.SetItem(ctx, user, inventory.Item{OfferID: offer, Quantity: 2})
	if err != nil || len(c.Items) != 1 || !c.Items[0].Reserved || c.SubtotalKurus != 2500 {
		t.Fatalf("sepet: %+v %v", c, err)
	}
	// Adet değişince eski rezervasyon bırakılır, yenisi alınır; toplam tutulan = yeni adet.
	if _, err := s.SetItem(ctx, user, inventory.Item{OfferID: offer, Quantity: 4}); err != nil {
		t.Fatal(err)
	}
	if _, reserved := fx.Stock(t, pool, offer); reserved != 4 || fx.ActiveReserved(t, pool, offer) != 4 {
		t.Fatalf("reserved=%d", reserved)
	}
	// Stoktan fazlası istenirse sepet değişmez, önceki rezervasyon korunur.
	if _, err := s.SetItem(ctx, user, inventory.Item{OfferID: offer, Quantity: 6}); !errors.Is(err, inventory.ErrInsufficientStock) {
		t.Fatalf("yetersiz stok bekleniyordu: %v", err)
	}
	if _, reserved := fx.Stock(t, pool, offer); reserved != 4 {
		t.Fatalf("başarısız güncelleme önceki rezervasyonu bozmamalı: %d", reserved)
	}
}

func TestRemoveItem_ReleasesStock(t *testing.T) {
	s, pool := newService(t, inventory.PolicyAllowPartial)
	ctx := context.Background()
	user, other := fx.User(t, pool, "customer"), fx.User(t, pool, "customer")
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 5)
	c, _ := s.SetItem(ctx, user, inventory.Item{OfferID: offer, Quantity: 3})

	if _, err := s.RemoveItem(ctx, other, c.Items[0].ID); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("başkasının kalemi silinememeli: %v", err)
	}
	c, err := s.RemoveItem(ctx, user, c.Items[0].ID)
	if err != nil || len(c.Items) != 0 {
		t.Fatalf("%+v %v", c, err)
	}
	if _, reserved := fx.Stock(t, pool, offer); reserved != 0 {
		t.Fatal("silinen kalemin stoğu iade edilmeli")
	}
}

func TestCheckout_MovesReservationsToOrder(t *testing.T) {
	s, pool := newService(t, inventory.PolicyAllowPartial)
	ctx := context.Background()
	user := fx.User(t, pool, "customer")
	a := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 5)
	b := fx.Offer(t, pool, fx.Seller(t, pool, ""), 2000, 5)
	_, _ = s.SetItem(ctx, user, inventory.Item{OfferID: a, Quantity: 2})
	_, _ = s.SetItem(ctx, user, inventory.Item{OfferID: b, Quantity: 1})

	o, err := s.Checkout(ctx, user, "")
	if err != nil || len(o.SubOrders) != 2 || o.TotalKurus != 4000 {
		t.Fatalf("%+v %v", o, err)
	}
	// Stok iki kez sayılmamalı: sepet rezervasyonu bırakıldı, sipariş rezervasyonu alındı.
	if _, reserved := fx.Stock(t, pool, a); reserved != 2 || fx.ActiveReserved(t, pool, a) != 2 {
		t.Fatalf("a reserved=%d", reserved)
	}
	c, _ := s.Get(ctx, user)
	if len(c.Items) != 0 {
		t.Fatal("sepet boşalmalı")
	}
	if _, err := s.Checkout(ctx, user, ""); !errors.Is(err, ErrEmpty) {
		t.Fatalf("boş sepet: %v", err)
	}
}

// Süresi dolan sepet rezervasyonu ödeme adımında yeniden alınır.
func TestCheckout_ReReservesExpiredItems(t *testing.T) {
	s, pool := newService(t, inventory.PolicyAllowPartial)
	ctx := context.Background()
	user := fx.User(t, pool, "customer")
	offer := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 5)
	s.now = func() time.Time { return time.Now().Add(-time.Hour) } // rezervasyon geçmişte bitsin
	_, _ = s.SetItem(ctx, user, inventory.Item{OfferID: offer, Quantity: 2})
	s.now = time.Now

	tx, _ := pool.Begin(ctx)
	if n, err := inventory.ExpireCartReservations(ctx, tx, time.Now(), 10); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	_ = tx.Commit(ctx)

	c, _ := s.Get(ctx, user)
	if len(c.Items) != 1 || c.Items[0].Reserved {
		t.Fatal("kalem sepette kalmalı ama rezervasyonsuz görünmeli")
	}
	if _, err := s.Checkout(ctx, user, ""); err != nil {
		t.Fatalf("stok varsa yeniden rezerve edilmeli: %v", err)
	}
	if _, reserved := fx.Stock(t, pool, offer); reserved != 2 {
		t.Fatalf("reserved=%d", reserved)
	}
}

// reject politikasında bir kalem bile yetmezse sepet olduğu gibi kalır.
func TestCheckout_RejectPolicyKeepsCartIntact(t *testing.T) {
	s, pool := newService(t, inventory.PolicyReject)
	ctx := context.Background()
	user, rival := fx.User(t, pool, "customer"), fx.User(t, pool, "customer")
	a := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 5)
	b := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 2)
	_, _ = s.SetItem(ctx, user, inventory.Item{OfferID: a, Quantity: 1})
	s.now = func() time.Time { return time.Now().Add(-time.Hour) }
	_, _ = s.SetItem(ctx, user, inventory.Item{OfferID: b, Quantity: 2})
	s.now = time.Now
	tx, _ := pool.Begin(ctx)
	_, _ = inventory.ExpireCartReservations(ctx, tx, time.Now(), 10)
	_ = tx.Commit(ctx)
	// Rakip müşteri b'nin stoğunu kapar.
	if _, err := s.SetItem(ctx, rival, inventory.Item{OfferID: b, Quantity: 2}); err != nil {
		t.Fatal(err)
	}

	_, err := s.Checkout(ctx, user, "")
	if !errors.Is(err, order.ErrInsufficientStock) {
		t.Fatalf("yetersiz stok bekleniyordu: %v", err)
	}
	c, _ := s.Get(ctx, user)
	if len(c.Items) != 2 {
		t.Fatal("sepet olduğu gibi kalmalı")
	}
	if _, reserved := fx.Stock(t, pool, a); reserved != 1 {
		t.Fatalf("a'nın sepet rezervasyonu geri alınmamalı: %d", reserved)
	}
}

// allow politikasında satılamayan kalem sepette kalır, diğerleri siparişe döner.
func TestCheckout_AllowPolicyLeavesUnavailableInCart(t *testing.T) {
	s, pool := newService(t, inventory.PolicyAllowPartial)
	ctx := context.Background()
	user, rival := fx.User(t, pool, "customer"), fx.User(t, pool, "customer")
	a := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 5)
	b := fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 2)
	_, _ = s.SetItem(ctx, user, inventory.Item{OfferID: a, Quantity: 1})
	s.now = func() time.Time { return time.Now().Add(-time.Hour) }
	_, _ = s.SetItem(ctx, user, inventory.Item{OfferID: b, Quantity: 2})
	s.now = time.Now
	tx, _ := pool.Begin(ctx)
	_, _ = inventory.ExpireCartReservations(ctx, tx, time.Now(), 10)
	_ = tx.Commit(ctx)
	_, _ = s.SetItem(ctx, rival, inventory.Item{OfferID: b, Quantity: 2})

	o, err := s.Checkout(ctx, user, "")
	if err != nil || len(o.SubOrders) != 1 || len(o.Unavailable) != 1 || o.Unavailable[0].OfferID != b {
		t.Fatalf("%+v %v", o, err)
	}
	c, _ := s.Get(ctx, user)
	if len(c.Items) != 1 || c.Items[0].OfferID != b || c.Items[0].Reserved {
		t.Fatalf("satılamayan kalem sepette kalmalı: %+v", c.Items)
	}
}
