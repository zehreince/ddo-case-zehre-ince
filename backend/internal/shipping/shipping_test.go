package shipping

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/inventory"
	"carsihub/internal/order"
	"carsihub/internal/outbox"
	"carsihub/internal/platform/dbtest"
	fx "carsihub/internal/platform/testfixture"
)

type fakeCarrier struct {
	mu    sync.Mutex
	calls map[string]int
	fail  bool
}

func (f *fakeCarrier) Create(_ context.Context, subOrderID, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return "", errors.New("kargo firması kapalı")
	}
	f.calls[subOrderID]++
	return "TRK-" + subOrderID[:8], nil
}

// paidOrder ödenmiş bir sipariş ve order.paid olayını hazırlar.
func paidOrder(t *testing.T, pool *pgxpool.Pool) (subOrderID string, ev outbox.Event) {
	t.Helper()
	ctx := context.Background()
	orders := order.NewService(pool, inventory.PolicyAllowPartial, time.Hour)
	customer := fx.User(t, pool, "customer")
	o, err := orders.Place(ctx, customer, []inventory.Item{{OfferID: fx.Offer(t, pool, fx.Seller(t, pool, ""), 1000, 5), Quantity: 1}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := orders.MarkPaid(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id, payload FROM outbox_events WHERE event_type = 'order.paid' AND aggregate_id = $1`, o.ID).
		Scan(&ev.ID, &ev.Payload); err != nil {
		t.Fatal(err)
	}
	return o.SubOrders[0].ID, ev
}

func TestConsumer_CreatesShipmentOnceAndRetries(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	carrier := &fakeCarrier{calls: map[string]int{}, fail: true}
	s := NewService(pool, carrier, "http://api/cb")
	sub, ev := paidOrder(t, pool)

	if err := s.Consumer().Handle(ctx, ev); err == nil {
		t.Fatal("kargo firması kapalıyken hata dönmeli (outbox yeniden denesin)")
	}
	carrier.fail = false
	for range 3 {
		if err := s.Consumer().Handle(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if carrier.calls[sub] != 1 {
		t.Fatalf("gönderi bir kez oluşturulmalı: %d", carrier.calls[sub])
	}
	var status, tracking string
	_ = pool.QueryRow(ctx, `SELECT status, tracking_number FROM shipments WHERE sub_order_id = $1::uuid`, sub).Scan(&status, &tracking)
	if status != "created" || tracking == "" {
		t.Fatalf("gönderi: %s %s", status, tracking)
	}
}

func TestApplyEvent_DedupAndForwardOnly(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	s := NewService(pool, &fakeCarrier{calls: map[string]int{}}, "")
	sub, ev := paidOrder(t, pool)
	if err := s.Consumer().Handle(ctx, ev); err != nil {
		t.Fatal(err)
	}
	tracking := "TRK-" + sub[:8]
	at := time.Now().UTC()

	apply := func(id, status string, offset time.Duration) bool {
		dup, err := s.ApplyEvent(ctx, Event{EventID: id, TrackingNumber: tracking, Status: status, OccurredAt: at.Add(offset)}, "test")
		if err != nil {
			t.Fatal(err)
		}
		return dup
	}
	apply("e1", "picked_up", 0)
	apply("e3", "delivered", 2*time.Minute)
	if !apply("e3", "delivered", 2*time.Minute) {
		t.Fatal("aynı event_id tekrarı duplicate sayılmalı")
	}
	apply("e2", "in_transit", time.Minute) // geç gelen eski olay: kayıt olur, durumu geri almaz

	sh, err := s.Track(ctx, tracking)
	if err != nil {
		t.Fatal(err)
	}
	if sh.Status != "delivered" || sh.DeliveredAt == nil || len(sh.Events) != 3 || sh.Events[1].Status != "in_transit" {
		t.Fatalf("takip: %+v", sh)
	}
	var subStatus string
	var delivered int
	_ = pool.QueryRow(ctx, `SELECT status FROM sub_orders WHERE id = $1::uuid`, sub).Scan(&subStatus)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE event_type = $1`, EventDelivered).Scan(&delivered)
	if subStatus != "delivered" || delivered != 1 {
		t.Fatalf("alt sipariş=%s teslim olayı=%d", subStatus, delivered)
	}

	if _, err := s.ApplyEvent(ctx, Event{EventID: "x", TrackingNumber: tracking, Status: "kayboldu"}, "test"); !errors.Is(err, ErrInvalidStatus) {
		t.Fatalf("geçersiz durum: %v", err)
	}
	if _, err := s.ApplyEvent(ctx, Event{EventID: "y", TrackingNumber: "YOK", Status: "picked_up"}, "test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bilinmeyen takip no: %v", err)
	}
	if _, err := s.Track(ctx, "YOK"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bilinmeyen takip: %v", err)
	}
}
