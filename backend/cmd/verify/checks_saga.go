package main

import (
	"fmt"
	"net/http"
	"time"

	"carsihub/internal/auth"
)

// Simülatörün kuruş hanesine göre deterministik davranışları (bkz. internal/simulator).
const (
	priceSuccess   = 12305 // ,05 → her zaman başarılı
	priceDecline   = 12301 // ,01 → reddedilir
	priceLost      = 12302 // ,02 → başarılı ama callback kaybolur
	priceDuplicate = 12303 // ,03 → callback iki kez gelir
	priceHang      = 12304 // ,04 → ödeme hiç sonuçlanmaz
)

type sagaOrder struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Payment *struct {
		Status        string `json:"status"`
		CallbackCount int    `json:"callback_count"`
	} `json:"payment"`
	SubOrders []struct {
		Status   string `json:"status"`
		Shipment *struct {
			TrackingNumber *string `json:"tracking_number"`
			Status         string  `json:"status"`
		} `json:"shipment"`
	} `json:"sub_orders"`
}

func (o sagaOrder) paymentStatus() string {
	if o.Payment == nil {
		return ""
	}
	return o.Payment.Status
}

func (o sagaOrder) String() string {
	ship := ""
	if len(o.SubOrders) > 0 && o.SubOrders[0].Shipment != nil {
		ship = o.SubOrders[0].Shipment.Status
	}
	sub := ""
	if len(o.SubOrders) > 0 {
		sub = o.SubOrders[0].Status
	}
	return fmt.Sprintf("sipariş=%s ödeme=%s alt=%s kargo=%s", o.Status, o.paymentStatus(), sub, ship)
}

// placeAt satıcı adına verilen fiyatta tek adetlik ürün açar ve müşteri olarak sipariş verir.
func (c *client) placeAt(seller, customer session, price int64) (orderID, offerID string, err error) {
	if offerID, err = c.newOffer(seller.AccessToken, price, 10); err != nil {
		return "", "", err
	}
	r, err := c.doKey(http.MethodPost, "/api/v1/orders", customer.AccessToken, fmt.Sprintf("saga-%d", time.Now().UnixNano()), orderBody(offerID, 1))
	if err != nil {
		return "", "", err
	}
	if err := r.expect(http.StatusCreated, ""); err != nil {
		return "", "", err
	}
	var o sagaOrder
	if err := r.decode(&o); err != nil {
		return "", "", err
	}
	return o.ID, offerID, nil
}

func (c *client) order(token, id string) (sagaOrder, error) {
	r, err := c.get("/api/v1/orders/"+id, token)
	if err != nil {
		return sagaOrder{}, err
	}
	var o sagaOrder
	if err := r.expect(http.StatusOK, ""); err != nil {
		return o, err
	}
	return o, r.decode(&o)
}

// waitOrder koşul sağlanana kadar siparişi yoklar.
func (c *client) waitOrder(token, id string, timeout time.Duration, done func(sagaOrder) bool) (sagaOrder, time.Duration, error) {
	start := time.Now()
	var last sagaOrder
	for time.Since(start) < timeout {
		o, err := c.order(token, id)
		if err != nil {
			return o, 0, err
		}
		if done(o) {
			return o, time.Since(start), nil
		}
		last = o
		time.Sleep(500 * time.Millisecond)
	}
	return last, 0, fmt.Errorf("%v içinde tamamlanmadı: %s", timeout, last)
}

func sessions(c *client) (seller, customer session, err error) {
	if seller, err = c.loginAs(auth.RoleSeller); err != nil {
		return
	}
	customer, err = c.loginAs(auth.RoleCustomer)
	return
}

var sagaChecks = []check{
	{"F-4/F-6", "ödeme başarılı → stok düşer → satıcıya bildirim → kargo teslim edilir", func(c *client) error {
		seller, customer, err := sessions(c)
		if err != nil {
			return err
		}
		id, offer, err := c.placeAt(seller, customer, priceSuccess)
		if err != nil {
			return err
		}
		o, took, err := c.waitOrder(customer.AccessToken, id, 45*time.Second, func(o sagaOrder) bool {
			return len(o.SubOrders) > 0 && o.SubOrders[0].Status == "delivered"
		})
		if err != nil {
			return err
		}
		if st, _ := c.offerStock(seller.AccessToken, offer); st.StockOnHand != 9 || st.Reserved != 0 {
			return fmt.Errorf("ödeme sonrası stok eldekinden düşmeli: %+v", st)
		}
		if o.Status != "paid" || o.paymentStatus() != "succeeded" {
			return fmt.Errorf("beklenmeyen durum: %s", o)
		}
		// Kargo takibi: olaylar sıralı, tekrar gelen "delivered" bir kez kayıtlı.
		r, err := c.get("/api/v1/shipments/"+*o.SubOrders[0].Shipment.TrackingNumber, customer.AccessToken)
		if err != nil {
			return err
		}
		var tr struct {
			Events []struct{ Status string } `json:"events"`
		}
		if err := r.decode(&tr); err != nil {
			return err
		}
		if len(tr.Events) != 3 || tr.Events[2].Status != "delivered" {
			return fmt.Errorf("3 kargo olayı bekleniyordu (tekrar eden teslim tekilleşmeli): %+v", tr.Events)
		}
		// Satıcı bildirimi
		r, err = c.get("/api/v1/seller/notifications?limit=50", seller.AccessToken)
		if err != nil {
			return err
		}
		var n struct {
			Items []struct {
				OrderID string `json:"order_id"`
			} `json:"items"`
		}
		if err := r.decode(&n); err != nil {
			return err
		}
		for _, it := range n.Items {
			if it.OrderID == id {
				return logf("teslim %v", took.Round(100*time.Millisecond))
			}
		}
		return fmt.Errorf("satıcıya bildirim düşmedi")
	}},
	{"T-2a", "ödeme reddi → telafi: sipariş iptal, stok iade", func(c *client) error {
		seller, customer, err := sessions(c)
		if err != nil {
			return err
		}
		id, offer, err := c.placeAt(seller, customer, priceDecline)
		if err != nil {
			return err
		}
		if _, _, err := c.waitOrder(customer.AccessToken, id, 20*time.Second, func(o sagaOrder) bool {
			return o.Status == "cancelled" && o.paymentStatus() == "failed"
		}); err != nil {
			return err
		}
		if st, _ := c.offerStock(seller.AccessToken, offer); st.StockOnHand != 10 || st.Reserved != 0 {
			return fmt.Errorf("stok iade edilmeli: %+v", st)
		}
		return nil
	}},
	{"T-2b", "çift callback → etki bir kez (stok bir kez düşer)", func(c *client) error {
		seller, customer, err := sessions(c)
		if err != nil {
			return err
		}
		id, offer, err := c.placeAt(seller, customer, priceDuplicate)
		if err != nil {
			return err
		}
		if _, _, err := c.waitOrder(customer.AccessToken, id, 20*time.Second, func(o sagaOrder) bool {
			return o.Payment != nil && o.Payment.CallbackCount >= 2
		}); err != nil {
			return err
		}
		if st, _ := c.offerStock(seller.AccessToken, offer); st.StockOnHand != 9 || st.Reserved != 0 {
			return fmt.Errorf("stok yalnızca bir kez düşmeli: %+v", st)
		}
		return nil
	}},
	{"T-2c", "callback kaybolur (zaman aşımı) → mutabakat ödemeyi tamamlar", func(c *client) error {
		seller, customer, err := sessions(c)
		if err != nil {
			return err
		}
		id, _, err := c.placeAt(seller, customer, priceLost)
		if err != nil {
			return err
		}
		o, took, err := c.waitOrder(customer.AccessToken, id, 60*time.Second, func(o sagaOrder) bool { return o.Status == "paid" })
		if err != nil {
			return err
		}
		if o.Payment.CallbackCount != 0 {
			return fmt.Errorf("callback gelmemeliydi: %d", o.Payment.CallbackCount)
		}
		return logf("mutabakatla %v", took.Round(100*time.Millisecond))
	}},
	{"T-2d", "iptal edilen siparişe geç gelen ödeme → telafi: iade", func(c *client) error {
		seller, customer, err := sessions(c)
		if err != nil {
			return err
		}
		id, offer, err := c.placeAt(seller, customer, priceLost)
		if err != nil {
			return err
		}
		// Ödeme sağlayıcıya ulaşsın (aksi halde sistem sağlayıcıya hiç gitmez ve
		// iadeye gerek kalmaz). Sağlayıcı parayı çeker ama callback gelmez;
		// müşteri bu arada iptal eder.
		if _, _, err := c.waitOrder(customer.AccessToken, id, 20*time.Second, func(o sagaOrder) bool {
			return o.paymentStatus() == "requested"
		}); err != nil {
			return err
		}
		r, err := c.do(http.MethodPost, "/api/v1/orders/"+id+"/cancel", customer.AccessToken, nil)
		if err != nil {
			return err
		}
		if err := r.expect(http.StatusOK, ""); err != nil {
			return err
		}
		o, _, err := c.waitOrder(customer.AccessToken, id, 60*time.Second, func(o sagaOrder) bool { return o.paymentStatus() == "refunded" })
		if err != nil {
			return err
		}
		if o.Status != "cancelled" {
			return fmt.Errorf("sipariş iptal kalmalı: %s", o)
		}
		if st, _ := c.offerStock(seller.AccessToken, offer); st.StockOnHand != 10 || st.Reserved != 0 {
			return fmt.Errorf("stok iade edilmiş kalmalı: %+v", st)
		}
		return nil
	}},
	{"T-2e", "outbox'ta bırakılmış (dead) olay yok; operasyon görünümü çalışır", func(c *client) error {
		ops, err := c.loginAs(auth.RoleOperations)
		if err != nil {
			return err
		}
		r, err := c.get("/api/v1/ops/outbox", ops.AccessToken)
		if err != nil {
			return err
		}
		var s struct {
			Pending, Retrying, Dead int
		}
		if err := r.expect(http.StatusOK, ""); err != nil {
			return err
		}
		if err := r.decode(&s); err != nil {
			return err
		}
		if s.Dead != 0 {
			return fmt.Errorf("%d olay bırakılmış: %s", s.Dead, truncate(r.body))
		}
		customer, _ := c.loginAs(auth.RoleCustomer)
		r, err = c.get("/api/v1/ops/outbox", customer.AccessToken)
		if err != nil {
			return err
		}
		if err := r.expect(http.StatusForbidden, "forbidden"); err != nil {
			return err
		}
		return logf("bekleyen=%d yeniden-deneniyor=%d", s.Pending, s.Retrying)
	}},
	{"WEBHOOK", "imzasız ödeme ve kargo callback'i reddedilir", func(c *client) error {
		for _, path := range []string{"/api/v1/payments/callback", "/api/v1/shipping/callback"} {
			r, err := c.do(http.MethodPost, path, "", map[string]string{"payment_id": "x", "status": "succeeded"})
			if err != nil {
				return err
			}
			if err := r.expect(http.StatusUnauthorized, "invalid_signature"); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
		}
		return nil
	}},
}

// info başarılı bir kontrolün ayrıntı sütununa yazılacak bilgidir (hata değildir).
type info string

func (i info) Error() string { return string(i) }

func logf(format string, args ...any) error { return info(fmt.Sprintf(format, args...)) }
