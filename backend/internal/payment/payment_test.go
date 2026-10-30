package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/inventory"
	"carsihub/internal/order"
	"carsihub/internal/outbox"
	"carsihub/internal/platform/dbtest"
	fx "carsihub/internal/platform/testfixture"
	"carsihub/internal/platform/webhook"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeProvider sağlayıcıyı taklit eder; çağrıları sayar ve hata enjekte edilebilir.
type fakeProvider struct {
	mu       sync.Mutex
	created  map[string]int
	refunds  map[string]int
	status   map[string]string
	failNext error
}

func newFake() *fakeProvider {
	return &fakeProvider{created: map[string]int{}, refunds: map[string]int{}, status: map[string]string{}}
}

func (f *fakeProvider) Create(_ context.Context, id string, _ int64, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failNext; err != nil {
		f.failNext = nil
		return err
	}
	f.created[id]++
	if _, ok := f.status[id]; !ok {
		f.status[id] = ProviderPending
	}
	return nil
}

func (f *fakeProvider) Status(_ context.Context, id string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.status[id]
	if !ok {
		return "", "", ErrProviderNotFound
	}
	return st, "", nil
}

func (f *fakeProvider) Refund(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refunds[id]++
	return nil
}

type env struct {
	pool     *pgxpool.Pool
	svc      *Service
	orders   *order.Service
	provider *fakeProvider
	customer string
	offer    string
}

func setup(t *testing.T) *env {
	pool := dbtest.New(t)
	orders := order.NewService(pool, inventory.PolicyAllowPartial, 15*time.Minute)
	p := newFake()
	return &env{
		pool: pool, orders: orders, provider: p,
		svc:      NewService(pool, orders, p, "http://api/cb", 0, discard),
		customer: fx.User(t, pool, "customer"),
		offer:    fx.Offer(t, pool, fx.Seller(t, pool, ""), 2500, 10),
	}
}

// place sipariş verir ve order.placed olayını (ödeme kimliğiyle) döner.
func (e *env) place(t *testing.T) (orderID, paymentID string, ev outbox.Event) {
	t.Helper()
	o, err := e.orders.Place(context.Background(), e.customer, []inventory.Item{{OfferID: e.offer, Quantity: 2}}, "")
	if err != nil {
		t.Fatal(err)
	}
	err = e.pool.QueryRow(context.Background(), `
		SELECT id, payload->>'payment_id', payload FROM outbox_events WHERE event_type = 'order.placed' AND aggregate_id = $1`, o.ID).
		Scan(&ev.ID, &paymentID, &ev.Payload)
	if err != nil {
		t.Fatal(err)
	}
	ev.Type = order.EventPlaced
	return o.ID, paymentID, ev
}

func (e *env) paymentStatus(t *testing.T, id string) Status {
	t.Helper()
	var s Status
	if err := e.pool.QueryRow(context.Background(), `SELECT status FROM payments WHERE id = $1::uuid`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *env) orderStatus(t *testing.T, id string) order.Status {
	t.Helper()
	o, err := e.orders.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return o.Status
}

func TestRequestConsumer_IsIdempotentAndRetriable(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	_, pay, ev := e.place(t)
	c := e.svc.RequestConsumer()

	e.provider.failNext = errors.New("sağlayıcı kapalı")
	if err := c.Handle(ctx, ev); err == nil {
		t.Fatal("sağlayıcı hatası outbox'a dönmeli (yeniden denensin)")
	}
	if e.paymentStatus(t, pay) != StatusPending {
		t.Fatal("başarısız istekten sonra ödeme beklemede kalmalı")
	}
	for range 3 { // en az bir kez teslim: aynı olay tekrar gelebilir
		if err := c.Handle(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if e.provider.created[pay] != 1 || e.paymentStatus(t, pay) != StatusRequested {
		t.Fatalf("sağlayıcıdan bir kez istenmeli: %d", e.provider.created[pay])
	}
}

func TestRequestConsumer_SkipsOrdersClosedBeforePayment(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	orderID, pay, ev := e.place(t)
	if _, err := e.orders.Cancel(ctx, e.customer, orderID, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RequestConsumer().Handle(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if e.provider.created[pay] != 0 || e.paymentStatus(t, pay) != StatusAbandoned {
		t.Fatal("iptal edilmiş sipariş için sağlayıcıya gidilmemeli")
	}
}

func TestApplyResult_SuccessFailureAndDuplicates(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	okOrder, okPay, _ := e.place(t)
	for i, want := range []Outcome{OutcomePaid, OutcomeDuplicate, OutcomeDuplicate} {
		got, err := e.svc.ApplyResult(ctx, okPay, ProviderSucceeded, "", "callback")
		if err != nil || got != want {
			t.Fatalf("callback %d: %s %v", i+1, got, err)
		}
	}
	// Başarı sonrası gelen çelişkili "başarısız" callback'i de etkisiz.
	if got, _ := e.svc.ApplyResult(ctx, okPay, ProviderFailed, "geç", "callback"); got != OutcomeDuplicate {
		t.Fatalf("son durum değişmemeli: %s", got)
	}
	if e.orderStatus(t, okOrder) != order.StatusPaid {
		t.Fatal("sipariş ödenmiş olmalı")
	}

	failOrder, failPay, _ := e.place(t)
	if got, _ := e.svc.ApplyResult(ctx, failPay, ProviderFailed, "kart reddedildi", "callback"); got != OutcomeFailed {
		t.Fatalf("ret: %s", got)
	}
	if e.orderStatus(t, failOrder) != order.StatusCancelled {
		t.Fatal("telafi: sipariş iptal edilmeli")
	}
	// 2 kesinleşti, 2 iade: eldeki 8, tutulan 0.
	if onHand, reserved := fx.Stock(t, e.pool, e.offer); onHand != 8 || reserved != 0 {
		t.Fatalf("stok: on_hand=%d reserved=%d", onHand, reserved)
	}
	var callbacks int
	_ = e.pool.QueryRow(ctx, `SELECT callback_count FROM payments WHERE id = $1::uuid`, okPay).Scan(&callbacks)
	if callbacks != 4 {
		t.Fatalf("callback sayısı: %d", callbacks)
	}
	if _, err := e.svc.ApplyResult(ctx, "00000000-0000-0000-0000-000000000000", ProviderSucceeded, "", "callback"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bilinmeyen ödeme: %v", err)
	}
	if _, err := e.svc.ApplyResult(ctx, okPay, "belki", "", "callback"); err == nil {
		t.Fatal("geçersiz sonuç reddedilmeli")
	}
}

// Aynı başarılı callback eşzamanlı 10 kez gelir: etki (stok düşümü) bir kez.
func TestApplyResult_ConcurrentCallbacks(t *testing.T) {
	e := setup(t)
	_, pay, _ := e.place(t)
	var paid, dup atomic.Int64
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := e.svc.ApplyResult(context.Background(), pay, ProviderSucceeded, "", "callback")
			switch {
			case err != nil:
				t.Error(err)
			case got == OutcomePaid:
				paid.Add(1)
			case got == OutcomeDuplicate:
				dup.Add(1)
			}
		}()
	}
	wg.Wait()
	if paid.Load() != 1 || dup.Load() != 9 {
		t.Fatalf("paid=%d duplicate=%d", paid.Load(), dup.Load())
	}
	if onHand, _ := fx.Stock(t, e.pool, e.offer); onHand != 8 {
		t.Fatalf("stok bir kez düşmeli: %d", onHand)
	}
}

// Sipariş kapandıktan sonra gelen başarılı ödeme → telafi: iade.
func TestLateSuccess_TriggersRefundOnce(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	orderID, pay, _ := e.place(t)
	if _, err := e.orders.Cancel(ctx, e.customer, orderID, ""); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.ApplyResult(ctx, pay, ProviderSucceeded, "", "callback")
	if err != nil || got != OutcomeRefunding || e.paymentStatus(t, pay) != StatusRefundRequested {
		t.Fatalf("iade başlamalı: %s %v", got, err)
	}
	var ev outbox.Event
	if err := e.pool.QueryRow(ctx, `SELECT id, payload FROM outbox_events WHERE event_type = $1`, EventRefundRequested).
		Scan(&ev.ID, &ev.Payload); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := e.svc.RefundConsumer().Handle(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if e.paymentStatus(t, pay) != StatusRefunded || e.orderStatus(t, orderID) != order.StatusCancelled {
		t.Fatal("ödeme iade edilmiş, sipariş iptal kalmalı")
	}
	if onHand, reserved := fx.Stock(t, e.pool, e.offer); onHand != 10 || reserved != 0 {
		t.Fatalf("stok dokunulmamalı: %d/%d", onHand, reserved)
	}
}

func TestReconcile(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	request := func() (string, string) {
		orderID, pay, ev := e.place(t)
		if err := e.svc.RequestConsumer().Handle(ctx, ev); err != nil {
			t.Fatal(err)
		}
		return orderID, pay
	}
	lostOrder, lost := request()      // sağlayıcıda başarılı, callback kayıp
	_, forgotten := request()         // sağlayıcı ödemeyi unuttu
	closedOrder, hanging := request() // sağlayıcıda askıda, sipariş kapandı
	e.provider.status[lost] = ProviderSucceeded
	delete(e.provider.status, forgotten)
	if _, err := e.orders.Cancel(ctx, e.customer, closedOrder, ""); err != nil {
		t.Fatal(err)
	}

	n, err := e.svc.Reconcile(ctx, 10)
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if e.orderStatus(t, lostOrder) != order.StatusPaid {
		t.Fatal("kayıp callback mutabakatla tamamlanmalı")
	}
	if e.provider.created[forgotten] != 2 {
		t.Fatalf("unutulan ödeme yeniden istenmeli: %d", e.provider.created[forgotten])
	}
	if e.paymentStatus(t, hanging) != StatusAbandoned {
		t.Fatal("kapanmış siparişin askıdaki ödemesi terk edilmeli")
	}
	// Terk edilen ödeme sonradan başarılı gelirse iade edilir.
	if got, _ := e.svc.ApplyResult(ctx, hanging, ProviderSucceeded, "", "callback"); got != OutcomeRefunding {
		t.Fatalf("geç başarı → iade: %s", got)
	}
}

func TestCallbackHandler_RequiresValidSignature(t *testing.T) {
	e := setup(t)
	secret := []byte("0123456789abcdef0123456789abcdef")
	r := chi.NewRouter()
	NewHandler(e.svc, secret, discard).Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()
	_, pay, _ := e.place(t)

	post := func(body []byte, sign bool) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/payments/callback", bytes.NewReader(body))
		if sign {
			webhook.SetHeaders(req.Header, secret, body, time.Now())
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	body, _ := json.Marshal(map[string]string{"payment_id": pay, "status": "succeeded"})
	if code := post(body, false); code != http.StatusUnauthorized {
		t.Fatalf("imzasız: %d", code)
	}
	if code := post([]byte(`{"payment_id":"x","status":"succeeded"}`), true); code != http.StatusUnprocessableEntity {
		t.Fatalf("geçersiz kimlik: %d", code)
	}
	unknown, _ := json.Marshal(map[string]string{"payment_id": "00000000-0000-0000-0000-000000000000", "status": "succeeded"})
	if code := post(unknown, true); code != http.StatusNotFound {
		t.Fatalf("bilinmeyen ödeme: %d", code)
	}
	if code := post(body, true); code != http.StatusOK {
		t.Fatalf("geçerli: %d", code)
	}
	if e.paymentStatus(t, pay) != StatusSucceeded {
		t.Fatal("ödeme başarılı olmalı")
	}
}
