package server_test

// Tüm modülleri gerçek router, gerçek token ve gerçek PostgreSQL ile HTTP
// üzerinden dener: rotalar, RBAC, hata biçimi ve idempotency birlikte.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"carsihub/internal/auth"
	"carsihub/internal/cart"
	"carsihub/internal/catalog"
	"carsihub/internal/coupon"
	"carsihub/internal/inventory"
	"carsihub/internal/notification"
	"carsihub/internal/ops"
	"carsihub/internal/order"
	"carsihub/internal/outbox"
	"carsihub/internal/payment"
	"carsihub/internal/platform/config"
	"carsihub/internal/platform/dbtest"
	"carsihub/internal/platform/idempotency"
	"carsihub/internal/platform/metrics"
	fx "carsihub/internal/platform/testfixture"
	"carsihub/internal/pricing"
	"carsihub/internal/server"
	"carsihub/internal/shipping"
)

const secret = "0123456789abcdef0123456789abcdef"

type api struct {
	t      *testing.T
	url    string
	pool   *pgxpool.Pool
	orders *order.Service
	ship   *shipping.Service
}

// stubCarrier kargo firmasını taklit eder: takip numarası alt sipariş kimliğinden türetilir.
type stubCarrier struct{}

func (stubCarrier) Create(_ context.Context, subOrderID, _ string) (string, error) {
	return "TRK-" + subOrderID[:8], nil
}

// stubProvider ödeme sağlayıcıyı taklit eder (bu testlerde çağrılmaz).
type stubProvider struct{}

func (stubProvider) Create(context.Context, string, int64, string) error { return nil }
func (stubProvider) Status(context.Context, string) (string, string, error) {
	return payment.ProviderPending, "", nil
}
func (stubProvider) Refund(context.Context, string) error { return nil }

func newAPI(t *testing.T) *api {
	pool := dbtest.New(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tokens := auth.NewTokens(secret, time.Minute)
	idem := idempotency.Middleware(pool, func(ctx context.Context) (string, bool) {
		p, ok := auth.PrincipalFrom(ctx)
		return p.UserID, ok
	}, log)
	orders := order.NewService(pool, inventory.PolicyAllowPartial, 15*time.Minute)
	ship := shipping.NewService(pool, stubCarrier{}, "")
	pay := payment.NewService(pool, orders, stubProvider{}, "", time.Minute, log)
	h := server.NewRouter(server.Deps{
		Config: config.Config{RequestTimeout: 10 * time.Second}, Log: log, DB: pool, Metrics: metrics.New(),
		Modules: []server.Module{
			auth.NewHandler(auth.NewService(pool, tokens, time.Hour), tokens, log, false),
			catalog.NewHandler(catalog.NewService(pool), tokens, log),
			cart.NewHandler(cart.NewService(pool, orders, 10*time.Minute), tokens, idem, log),
			order.NewHandler(orders, tokens, idem, log),
			payment.NewHandler(pay, []byte(secret), log),
			shipping.NewHandler(ship, tokens, []byte(secret), log),
			notification.NewHandler(pool, tokens, log),
			ops.NewHandler(pool, tokens, log),
			coupon.NewHandler(coupon.NewService(pool), tokens, log),
		},
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &api{t: t, url: srv.URL + "/api/v1", pool: pool, orders: orders, ship: ship}
}

// user verilen rolde giriş yapabilen bir kullanıcı oluşturur ve access token döner.
func (a *api) user(role auth.Role) (id, token string) {
	a.t.Helper()
	hash, _ := auth.HashPassword("Parola123!", bcrypt.MinCost)
	email := strings.ToLower(string(role)) + time.Now().Format("150405.000000") + "@test.co"
	if err := a.pool.QueryRow(context.Background(), `
		INSERT INTO users (email, password_hash, full_name, role) VALUES ($1, $2, 'T', $3) RETURNING id::text`,
		email, hash, role).Scan(&id); err != nil {
		a.t.Fatal(err)
	}
	var s struct {
		AccessToken string `json:"access_token"`
	}
	a.do("POST", "/auth/login", "", "", map[string]string{"email": email, "password": "Parola123!"}, http.StatusOK, &s)
	return id, s.AccessToken
}

func (a *api) do(method, path, token, idemKey string, body any, want int, out any) []byte {
	a.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.url+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idemKey != "" {
		req.Header.Set(idempotency.Header, idemKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		a.t.Fatalf("%s %s: durum %d, beklenen %d: %s", method, path, resp.StatusCode, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			a.t.Fatalf("json: %v: %s", err, raw)
		}
	}
	return raw
}

func TestAPI_SellerToCustomerFlow(t *testing.T) {
	a := newAPI(t)
	sellerUser, sellerTok := a.user(auth.RoleSeller)
	fx.Seller(t, a.pool, sellerUser)
	_, customerTok := a.user(auth.RoleCustomer)
	_, financeTok := a.user(auth.RoleFinance)
	category := fx.Category(t, a.pool)

	// Satıcı ürün ekler (2 varyant).
	var product catalog.Product
	a.do("POST", "/seller/products", sellerTok, "", map[string]any{
		"category_id": category, "name": "Pamuklu Tişört",
		"variants": []map[string]any{
			{"sku": "TS-S", "attributes": map[string]string{"beden": "S"}, "price_kurus": 19990, "stock": 3},
			{"sku": "TS-M", "attributes": map[string]string{"beden": "M"}, "price_kurus": 19990, "stock": 0},
		},
	}, http.StatusCreated, &product)
	if len(product.Variants) != 2 || len(product.Variants[0].Offers) != 1 {
		t.Fatalf("ürün: %+v", product)
	}
	// Varyantlar SKU'ya göre sıralı döner: TS-M, TS-S
	if product.Variants[0].SKU != "TS-M" {
		t.Fatalf("varyantlar SKU'ya göre sıralı olmalı: %+v", product.Variants)
	}
	offerM := product.Variants[0].Offers[0].ID
	offerS := product.Variants[1].Offers[0].ID

	// Doğrulama ve RBAC
	a.do("POST", "/seller/products", sellerTok, "", map[string]any{"name": ""}, http.StatusUnprocessableEntity, nil)
	a.do("POST", "/seller/products", customerTok, "", map[string]any{}, http.StatusForbidden, nil)
	a.do("POST", "/cart/items", sellerTok, "", map[string]any{"offer_id": offerS, "quantity": 1}, http.StatusForbidden, nil)
	a.do("GET", "/orders", financeTok, "", nil, http.StatusForbidden, nil)
	a.do("GET", "/products", "", "", nil, http.StatusUnauthorized, nil)

	// Katalog
	var list struct {
		Items []catalog.ProductSummary `json:"items"`
	}
	a.do("GET", "/products?limit=5", customerTok, "", nil, http.StatusOK, &list)
	if len(list.Items) != 1 || list.Items[0].Available != 3 || *list.Items[0].MinPriceKurus != 19990 {
		t.Fatalf("liste: %+v", list.Items)
	}
	a.do("GET", "/products/bozuk-id", customerTok, "", nil, http.StatusNotFound, nil)
	var cats struct {
		Items []catalog.Category `json:"items"`
	}
	a.do("GET", "/categories", customerTok, "", nil, http.StatusOK, &cats)

	// Sepet: stoksuz varyant 409, stoklu olan eklenir. Aynı Idempotency-Key tekrarında işlem tekrarlanmaz.
	a.do("POST", "/cart/items", customerTok, "", map[string]any{"offer_id": offerM, "quantity": 1}, http.StatusConflict, nil)
	a.do("POST", "/cart/items", customerTok, "", map[string]any{"offer_id": offerS, "quantity": 0}, http.StatusUnprocessableEntity, nil)
	var c cart.Cart
	a.do("POST", "/cart/items", customerTok, "sepet-1", map[string]any{"offer_id": offerS, "quantity": 2}, http.StatusOK, &c)
	a.do("POST", "/cart/items", customerTok, "sepet-1", map[string]any{"offer_id": offerS, "quantity": 2}, http.StatusOK, &c)
	if len(c.Items) != 1 || !c.Items[0].Reserved || c.SubtotalKurus != 39980 {
		t.Fatalf("sepet: %+v", c)
	}
	if _, reserved := fx.Stock(t, a.pool, offerS); reserved != 2 {
		t.Fatalf("idempotent tekrar ikinci kez rezerve etmemeli: %d", reserved)
	}
	a.do("GET", "/cart", customerTok, "", nil, http.StatusOK, &c)

	// Satıcı, tutulan miktarın altına stok indiremez.
	a.do("PUT", "/seller/offers/"+offerS+"/stock", sellerTok, "", map[string]int{"stock_on_hand": 1}, http.StatusConflict, nil)
	var offers struct {
		Items []catalog.Offer `json:"items"`
	}
	a.do("GET", "/seller/offers", sellerTok, "", nil, http.StatusOK, &offers)
	if len(offers.Items) != 2 {
		t.Fatalf("satıcı teklifleri: %+v", offers)
	}

	// Ödeme adımı → sipariş
	var o order.Order
	a.do("POST", "/cart/checkout", customerTok, "odeme-1", nil, http.StatusCreated, &o)
	if o.Status != order.StatusPendingPayment || o.TotalKurus != 39980 || len(o.SubOrders) != 1 {
		t.Fatalf("sipariş: %+v", o)
	}
	a.do("POST", "/cart/checkout", customerTok, "", nil, http.StatusConflict, nil) // sepet boş

	// Doğrudan sipariş: stok yetmez → 409 ve hangi kalem olduğu
	raw := a.do("POST", "/orders", customerTok, "", map[string]any{
		"items": []map[string]any{{"offer_id": offerS, "quantity": 5}},
	}, http.StatusConflict, nil)
	if !strings.Contains(string(raw), `"unavailable_items"`) || !strings.Contains(string(raw), offerS) {
		t.Fatalf("409 ayrıntısı eksik: %s", raw)
	}

	// Sipariş görüntüleme ve iptal
	var got order.Order
	a.do("GET", "/orders/"+o.ID, customerTok, "", nil, http.StatusOK, &got)
	var page struct {
		Items []order.Order `json:"items"`
	}
	a.do("GET", "/orders", customerTok, "", nil, http.StatusOK, &page)
	if len(page.Items) != 1 {
		t.Fatalf("sipariş listesi: %+v", page)
	}
	_, otherTok := a.user(auth.RoleCustomer)
	a.do("GET", "/orders/"+o.ID, otherTok, "", nil, http.StatusNotFound, nil)
	a.do("POST", "/orders/"+o.ID+"/cancel", otherTok, "", nil, http.StatusNotFound, nil)
	a.do("POST", "/orders/"+o.ID+"/cancel", customerTok, "", nil, http.StatusOK, &got)
	a.do("POST", "/orders/"+o.ID+"/cancel", customerTok, "", nil, http.StatusConflict, nil)
	if got.Status != order.StatusCancelled {
		t.Fatalf("iptal: %+v", got)
	}
	if _, reserved := fx.Stock(t, a.pool, offerS); reserved != 0 {
		t.Fatal("iptal stoğu iade etmeli")
	}

	// Stok artık indirilebilir; denetim kaydı eski/yeni değeri tutar.
	a.do("PUT", "/seller/offers/"+offerS+"/stock", sellerTok, "", map[string]int{"stock_on_hand": 10}, http.StatusOK, nil)
	var oldNew string
	_ = a.pool.QueryRow(context.Background(), `
		SELECT old_value::text || ' -> ' || new_value::text FROM audit_log WHERE action = 'offer.stock_set'`).Scan(&oldNew)
	if !strings.Contains(oldNew, `"stock_on_hand": 3`) || !strings.Contains(oldNew, `"stock_on_hand": 10`) {
		t.Fatalf("audit eski/yeni değer: %s", oldNew)
	}
}

// Ödeme sonrası saga adımlarının HTTP yüzü: satıcı bildirimi, kargo rolü, takip ve operasyon.
func TestAPI_SagaEndpoints(t *testing.T) {
	a := newAPI(t)
	ctx := context.Background()
	sellerUser, sellerTok := a.user(auth.RoleSeller)
	sellerID := fx.Seller(t, a.pool, sellerUser)
	customerID, customerTok := a.user(auth.RoleCustomer)
	_, otherTok := a.user(auth.RoleCustomer)
	_, carrierTok := a.user(auth.RoleCarrier)
	_, opsTok := a.user(auth.RoleOperations)

	o, err := a.orders.Place(ctx, customerID, []inventory.Item{{OfferID: fx.Offer(t, a.pool, sellerID, 1500, 3), Quantity: 1}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.orders.MarkPaid(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	var ev outbox.Event
	if err := a.pool.QueryRow(ctx, `SELECT id, payload FROM outbox_events WHERE event_type = 'order.paid'`).Scan(&ev.ID, &ev.Payload); err != nil {
		t.Fatal(err)
	}
	for range 2 { // en az bir kez teslim: tekrar gelse de tek bildirim, tek gönderi
		if err := notification.NewConsumer(a.pool).Handle(ctx, ev); err != nil {
			t.Fatal(err)
		}
		if err := a.ship.Consumer().Handle(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}

	// Satıcı bildirimi
	var notes struct {
		Items []notification.Notification `json:"items"`
	}
	a.do("GET", "/seller/notifications", sellerTok, "", nil, http.StatusOK, &notes)
	if len(notes.Items) != 1 || notes.Items[0].OrderID != o.ID || notes.Items[0].TotalKurus != 1500 {
		t.Fatalf("bildirim: %+v", notes.Items)
	}
	a.do("GET", "/seller/notifications", customerTok, "", nil, http.StatusForbidden, nil)

	// Kargo entegrasyonu rolü olay girer; Idempotency-Key olay kimliğidir.
	tracking := "TRK-" + o.SubOrders[0].ID[:8]
	path := "/carrier/shipments/" + tracking + "/events"
	a.do("POST", path, carrierTok, "", map[string]string{"status": "picked_up"}, http.StatusUnprocessableEntity, nil)
	var res struct{ Duplicate bool }
	a.do("POST", path, carrierTok, "olay-1", map[string]string{"status": "picked_up"}, http.StatusOK, &res)
	a.do("POST", path, carrierTok, "olay-1", map[string]string{"status": "picked_up"}, http.StatusOK, &res)
	if !res.Duplicate {
		t.Fatal("aynı anahtarla ikinci olay duplicate olmalı")
	}
	a.do("POST", path, carrierTok, "olay-2", map[string]string{"status": "uçtu"}, http.StatusUnprocessableEntity, nil)
	a.do("POST", "/carrier/shipments/YOK/events", carrierTok, "olay-3", map[string]string{"status": "picked_up"}, http.StatusNotFound, nil)
	a.do("POST", path, customerTok, "olay-4", map[string]string{"status": "delivered"}, http.StatusForbidden, nil)

	// Takip: müşteri yalnızca kendi gönderisini görür.
	var sh shipping.Shipment
	a.do("GET", "/shipments/"+tracking, customerTok, "", nil, http.StatusOK, &sh)
	if sh.Status != "picked_up" || len(sh.Events) != 1 {
		t.Fatalf("takip: %+v", sh)
	}
	a.do("GET", "/shipments/"+tracking, otherTok, "", nil, http.StatusNotFound, nil)
	a.do("GET", "/shipments/YOK", carrierTok, "", nil, http.StatusNotFound, nil)
	var got order.Order
	a.do("GET", "/orders/"+o.ID, customerTok, "", nil, http.StatusOK, &got)
	if got.Payment == nil || got.SubOrders[0].Shipment == nil || got.SubOrders[0].Status != "shipped" {
		t.Fatalf("sipariş ayrıntısı saga durumunu göstermeli: %+v", got)
	}

	// İmzasız kargo callback'i reddedilir.
	a.do("POST", "/shipping/callback", "", "", map[string]string{"event_id": "x"}, http.StatusUnauthorized, nil)

	// Operasyon: outbox görünümü ve bırakılmış olayı yeniden kuyruğa alma.
	if _, err := a.pool.Exec(ctx, `UPDATE outbox_events SET dead = true, attempts = 10, last_error = 'kalıcı hata' WHERE id = $1`, ev.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.pool.Exec(ctx, `UPDATE outbox_events SET published_at = NULL WHERE id = $1`, ev.ID); err != nil {
		t.Fatal(err)
	}
	var summary struct {
		Dead     int
		Problems []struct{ ID int64 }
	}
	a.do("GET", "/ops/outbox", opsTok, "", nil, http.StatusOK, &summary)
	if summary.Dead != 1 || len(summary.Problems) != 1 {
		t.Fatalf("outbox özeti: %+v", summary)
	}
	a.do("GET", "/ops/outbox", customerTok, "", nil, http.StatusForbidden, nil)
	id := strconv.FormatInt(ev.ID, 10)
	a.do("POST", "/ops/outbox/"+id+"/retry", opsTok, "", nil, http.StatusNoContent, nil)
	a.do("POST", "/ops/outbox/"+id+"/retry", opsTok, "", nil, http.StatusConflict, nil)
	a.do("POST", "/ops/outbox/abc/retry", opsTok, "", nil, http.StatusNotFound, nil)
	a.do("GET", "/ops/outbox", opsTok, "", nil, http.StatusOK, &summary)
	if summary.Dead != 0 {
		t.Fatal("yeniden kuyruğa alınan olay bırakılmış sayılmamalı")
	}
}

// T-3'ün HTTP yüzü: rol bazlı kupon oluşturma, sepette kupon, indirimli ödeme ve iade önizlemesi.
func TestAPI_Coupons(t *testing.T) {
	a := newAPI(t)
	ctx := context.Background()
	sellerUser, sellerTok := a.user(auth.RoleSeller)
	sellerID := fx.Seller(t, a.pool, sellerUser)
	_, adminTok := a.user(auth.RoleAdmin)
	_, customerTok := a.user(auth.RoleCustomer)
	_, otherTok := a.user(auth.RoleCustomer)
	mine := fx.Offer(t, a.pool, sellerID, 20000, 10)                  // 200 TL
	other := fx.Offer(t, a.pool, fx.Seller(t, a.pool, ""), 10000, 10) // 100 TL

	// Kapsam rolden gelir: yönetici → pazaryeri, satıcı → kendi mağazası. Müşteri açamaz.
	var created coupon.Coupon
	a.do("POST", "/coupons", adminTok, "", map[string]any{"code": "pazar40", "kind": "fixed", "value": 4000, "min_basket_kurus": 25000}, http.StatusCreated, &created)
	if created.Scope != "marketplace" || created.Code != "PAZAR40" {
		t.Fatalf("pazaryeri kuponu: %+v", created)
	}
	a.do("POST", "/coupons", sellerTok, "", map[string]any{"code": "MAGAZA10", "kind": "percent", "value": 1000}, http.StatusCreated, &created)
	if created.Scope != "seller" || created.SellerID != sellerID {
		t.Fatalf("satıcı kuponu: %+v", created)
	}
	a.do("POST", "/coupons", sellerTok, "", map[string]any{"code": "MAGAZA10", "kind": "percent", "value": 1000}, http.StatusConflict, nil)
	a.do("POST", "/coupons", adminTok, "", map[string]any{"code": "x", "kind": "percent", "value": 0}, http.StatusUnprocessableEntity, nil)
	a.do("POST", "/coupons", customerTok, "", map[string]any{"code": "BENIM", "kind": "fixed", "value": 1}, http.StatusForbidden, nil)
	var list struct{ Items []coupon.Coupon }
	a.do("GET", "/coupons", sellerTok, "", nil, http.StatusOK, &list)
	if len(list.Items) != 1 || list.Items[0].Code != "MAGAZA10" {
		t.Fatalf("satıcı yalnızca kendi kuponlarını görür: %+v", list.Items)
	}
	a.do("GET", "/coupons?limit=1", adminTok, "", nil, http.StatusOK, &list)

	// Sepet: iki satıcı, iki kupon.
	a.do("POST", "/cart/items", customerTok, "", map[string]any{"offer_id": mine, "quantity": 1}, http.StatusOK, nil)
	a.do("POST", "/cart/items", customerTok, "", map[string]any{"offer_id": other, "quantity": 1}, http.StatusOK, nil)
	a.do("POST", "/cart/coupons", customerTok, "", map[string]string{"code": "YOKBOYLE"}, http.StatusNotFound, nil)
	a.do("POST", "/cart/coupons", customerTok, "", map[string]string{"code": ""}, http.StatusUnprocessableEntity, nil)
	var c cart.Cart
	a.do("POST", "/cart/coupons", customerTok, "", map[string]string{"code": "magaza10"}, http.StatusOK, &c)
	a.do("POST", "/cart/coupons", customerTok, "", map[string]string{"code": "PAZAR40"}, http.StatusOK, &c)
	// 300 TL: mağaza %10 → 20; pazaryeri 40 → toplam 60 indirim.
	if c.SubtotalKurus != 30000 || c.DiscountKurus != 6000 || c.TotalKurus != 24000 || len(c.AppliedCoupons) != 2 {
		t.Fatalf("sepet: %+v", c)
	}
	a.do("DELETE", "/cart/coupons/PAZAR40", customerTok, "", nil, http.StatusOK, &c)
	if c.DiscountKurus != 2000 || len(c.CouponCodes) != 1 {
		t.Fatalf("kupon çıkarılınca: %+v", c)
	}
	a.do("POST", "/cart/coupons", customerTok, "", map[string]string{"code": "PAZAR40"}, http.StatusOK, &c)

	var o order.Order
	a.do("POST", "/cart/checkout", customerTok, "", nil, http.StatusCreated, &o)
	if o.TotalKurus != 24000 || len(o.AppliedCoupons) != 2 {
		t.Fatalf("sipariş: %+v", o)
	}
	a.do("GET", "/cart", customerTok, "", nil, http.StatusOK, &c)
	if len(c.CouponCodes) != 0 {
		t.Fatal("kuponlar siparişte kullanıldı, sepette kalmamalı")
	}

	// Doğrudan siparişte kupon kodu sınırı
	codes := []string{"A", "B", "C", "D", "E", "F"}
	a.do("POST", "/orders", customerTok, "", map[string]any{"items": []map[string]any{{"offer_id": mine, "quantity": 1}}, "coupon_codes": codes}, http.StatusUnprocessableEntity, nil)

	// İade önizlemesi: ödeme sonrası. Müşteri yalnızca kendi siparişi için.
	a.do("POST", "/orders/"+o.ID+"/refund-quote", customerTok, "", map[string]any{"items": []any{}}, http.StatusConflict, nil)
	if err := a.orders.MarkPaid(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	var full order.Order
	a.do("GET", "/orders/"+o.ID, customerTok, "", nil, http.StatusOK, &full)
	var otherItem string
	for _, sub := range full.SubOrders {
		for _, l := range sub.Items {
			if l.OfferID == other {
				otherItem = l.ID
			}
		}
	}
	body := map[string]any{"items": []map[string]any{{"order_item_id": otherItem, "quantity": 1}}}
	var rq pricing.RefundQuote
	a.do("POST", "/orders/"+o.ID+"/refund-quote", customerTok, "", body, http.StatusOK, &rq)
	// Kalan 200 TL - %10 = 180 < 250 → pazaryeri kuponu düşer; 240 - 180 = 60 TL iade.
	if rq.RefundKurus != 6000 || len(rq.LostCoupons) != 1 || rq.LostCoupons[0] != "PAZAR40" {
		t.Fatalf("iade önizlemesi: %+v", rq)
	}
	a.do("POST", "/orders/"+o.ID+"/refund-quote", otherTok, "", body, http.StatusNotFound, nil)
	a.do("POST", "/orders/"+o.ID+"/refund-quote", customerTok, "", map[string]any{"items": []map[string]any{{"order_item_id": otherItem, "quantity": 5}}}, http.StatusUnprocessableEntity, nil)
}
