package main

import (
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"carsihub/internal/auth"
)

type orderView struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	TotalKurus int64  `json:"total_kurus"`
	SubOrders  []struct {
		SellerID   string `json:"seller_id"`
		TotalKurus int64  `json:"total_kurus"`
	} `json:"sub_orders"`
	Unavailable []struct {
		OfferID string `json:"offer_id"`
	} `json:"unavailable_items"`
}

type cartView struct {
	Items []struct {
		ID            string     `json:"id"`
		OfferID       string     `json:"offer_id"`
		Quantity      int        `json:"quantity"`
		Reserved      bool       `json:"reserved"`
		ReservedUntil *time.Time `json:"reserved_until"`
	} `json:"items"`
	SubtotalKurus int64 `json:"subtotal_kurus"`
}

// emptyCart müşterinin sepetini boşaltır; kontroller birbirini etkilemesin.
func (c *client) emptyCart(token string) error {
	r, err := c.get("/api/v1/cart", token)
	if err != nil {
		return err
	}
	var cv cartView
	if err := r.decode(&cv); err != nil {
		return err
	}
	for _, it := range cv.Items {
		if _, err := c.do(http.MethodDelete, "/api/v1/cart/items/"+it.ID, token, nil); err != nil {
			return err
		}
	}
	return nil
}

var inventoryChecks = []check{
	{"F-2", "sepete ekleme stok tutar (TTL); adet değişir, çıkarınca stok döner", func(c *client) error {
		seller, err := c.loginAs(auth.RoleSeller)
		if err != nil {
			return err
		}
		customer, err := c.loginAs(auth.RoleCustomer)
		if err != nil {
			return err
		}
		if err := c.emptyCart(customer.AccessToken); err != nil {
			return err
		}
		offerID, err := c.newOffer(seller.AccessToken, 1500, 10)
		if err != nil {
			return err
		}
		r, err := c.do(http.MethodPost, "/api/v1/cart/items", customer.AccessToken, map[string]any{"offer_id": offerID, "quantity": 3})
		if err != nil {
			return err
		}
		var cv cartView
		if err := r.decode(&cv); err != nil {
			return err
		}
		if len(cv.Items) != 1 || !cv.Items[0].Reserved || cv.Items[0].ReservedUntil == nil || cv.SubtotalKurus != 4500 {
			return fmt.Errorf("sepet: %s", truncate(r.body))
		}
		if ttl := time.Until(*cv.Items[0].ReservedUntil); ttl <= 0 || ttl > 24*time.Hour {
			return fmt.Errorf("rezervasyon süresi anlamsız: %v", ttl)
		}
		if o, _ := c.offerStock(seller.AccessToken, offerID); o.Reserved != 3 {
			return fmt.Errorf("sepet 3 birim tutmalı, reserved=%d", o.Reserved)
		}
		if _, err := c.do(http.MethodPost, "/api/v1/cart/items", customer.AccessToken, map[string]any{"offer_id": offerID, "quantity": 5}); err != nil {
			return err
		}
		if o, _ := c.offerStock(seller.AccessToken, offerID); o.Reserved != 5 {
			return fmt.Errorf("adet değişince reserved=5 olmalı, %d", o.Reserved)
		}
		if err := c.emptyCart(customer.AccessToken); err != nil {
			return err
		}
		if o, _ := c.offerStock(seller.AccessToken, offerID); o.Reserved != 0 {
			return fmt.Errorf("sepetten çıkınca stok dönmeli, reserved=%d", o.Reserved)
		}
		return nil
	}},
	{"F-3", "çok satıcılı sepet → tek sipariş, satıcı başına alt sipariş", func(c *client) error {
		seller, err := c.loginAs(auth.RoleSeller)
		if err != nil {
			return err
		}
		customer, err := c.loginAs(auth.RoleCustomer)
		if err != nil {
			return err
		}
		if err := c.emptyCart(customer.AccessToken); err != nil {
			return err
		}
		mine, err := c.newOffer(seller.AccessToken, 1000, 5)
		if err != nil {
			return err
		}
		other, err := c.otherSellerOffer(customer.AccessToken, seller)
		if err != nil {
			return err
		}
		for _, id := range []string{mine, other} {
			r, err := c.do(http.MethodPost, "/api/v1/cart/items", customer.AccessToken, map[string]any{"offer_id": id, "quantity": 1})
			if err != nil {
				return err
			}
			if err := r.expect(http.StatusOK, ""); err != nil {
				return err
			}
		}
		r, err := c.doKey(http.MethodPost, "/api/v1/cart/checkout", customer.AccessToken, fmt.Sprintf("vrf-%d", time.Now().UnixNano()), nil)
		if err != nil {
			return err
		}
		if err := r.expect(http.StatusCreated, ""); err != nil {
			return err
		}
		var o orderView
		if err := r.decode(&o); err != nil {
			return err
		}
		var sum int64
		for _, s := range o.SubOrders {
			sum += s.TotalKurus
		}
		if len(o.SubOrders) != 2 || o.SubOrders[0].SellerID == o.SubOrders[1].SellerID || sum != o.TotalKurus {
			return fmt.Errorf("iki satıcılı alt sipariş bekleniyordu: %s", truncate(r.body))
		}
		_, _ = c.do(http.MethodPost, "/api/v1/orders/"+o.ID+"/cancel", customer.AccessToken, nil)
		return nil
	}},
	{"T-1a", "1.000 eşzamanlı sipariş: stoktan fazlası satılmaz", func(c *client) error {
		seller, err := c.loginAs(auth.RoleSeller)
		if err != nil {
			return err
		}
		customer, err := c.loginAs(auth.RoleCustomer)
		if err != nil {
			return err
		}
		const stock, requests = 50, 1000
		offerID, err := c.newOffer(seller.AccessToken, 999, stock)
		if err != nil {
			return err
		}
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			created []string
			codes   = map[int]int{}
		)
		start := make(chan struct{})
		run := time.Now().UnixNano()
		for i := range requests {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				r, err := c.doKey(http.MethodPost, "/api/v1/orders", customer.AccessToken, fmt.Sprintf("t1-%d-%d", run, i), orderBody(offerID, 1))
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					codes[0]++
					return
				}
				codes[r.status]++
				if r.status == http.StatusCreated {
					var o orderView
					if r.decode(&o) == nil {
						created = append(created, o.ID)
					}
				}
			}()
		}
		began := time.Now()
		close(start)
		wg.Wait()
		elapsed := time.Since(began)

		o, err := c.offerStock(seller.AccessToken, offerID)
		if err != nil {
			return err
		}
		summary := fmt.Sprintf("201=%d 409=%d diğer=%d reserved=%d/%d süre=%v",
			codes[http.StatusCreated], codes[http.StatusConflict], requests-codes[http.StatusCreated]-codes[http.StatusConflict],
			o.Reserved, stock, elapsed.Round(time.Millisecond))
		if codes[http.StatusCreated] != stock || codes[http.StatusConflict] != requests-stock || o.Reserved != stock || o.Available != 0 {
			return fmt.Errorf("oversell kontrolü başarısız: %s", summary)
		}
		fmt.Fprintf(os.Stderr, "  T-1a: %s\n", summary)

		// Siparişler iptal edilince stoğun tamamı geri döner.
		for _, id := range created {
			if _, err := c.do(http.MethodPost, "/api/v1/orders/"+id+"/cancel", customer.AccessToken, nil); err != nil {
				return err
			}
		}
		if o, _ := c.offerStock(seller.AccessToken, offerID); o.Reserved != 0 || o.Available != stock {
			return fmt.Errorf("iptal sonrası stok iade edilmedi: %+v", o)
		}
		return nil
	}},
	{"T-1b", "kısmi satış kuralı (PARTIAL_FULFILLMENT) uygulanır", func(c *client) error {
		seller, err := c.loginAs(auth.RoleSeller)
		if err != nil {
			return err
		}
		customer, err := c.loginAs(auth.RoleCustomer)
		if err != nil {
			return err
		}
		plenty, err := c.newOffer(seller.AccessToken, 1000, 10)
		if err != nil {
			return err
		}
		scarce, err := c.newOffer(seller.AccessToken, 1000, 1)
		if err != nil {
			return err
		}
		r, err := c.do(http.MethodPost, "/api/v1/orders", customer.AccessToken, orderBody(plenty, 2, scarce, 3))
		if err != nil {
			return err
		}
		switch policy := os.Getenv("PARTIAL_FULFILLMENT"); policy {
		case "reject":
			if err := r.expect(http.StatusConflict, "insufficient_stock"); err != nil {
				return err
			}
			if o, _ := c.offerStock(seller.AccessToken, plenty); o.Reserved != 0 {
				return fmt.Errorf("reject: hiçbir kalem tutulmamalı, reserved=%d", o.Reserved)
			}
		default: // allow
			if err := r.expect(http.StatusCreated, ""); err != nil {
				return err
			}
			var o orderView
			if err := r.decode(&o); err != nil {
				return err
			}
			if o.TotalKurus != 2000 || len(o.Unavailable) != 1 || o.Unavailable[0].OfferID != scarce {
				return fmt.Errorf("allow: yalnızca stoklu kalem satılmalı: %s", truncate(r.body))
			}
			_, _ = c.do(http.MethodPost, "/api/v1/orders/"+o.ID+"/cancel", customer.AccessToken, nil)
		}
		return nil
	}},
	{"T-1c", "aynı Idempotency-Key ile tekrar sipariş tek kez işlenir", func(c *client) error {
		seller, err := c.loginAs(auth.RoleSeller)
		if err != nil {
			return err
		}
		customer, err := c.loginAs(auth.RoleCustomer)
		if err != nil {
			return err
		}
		offerID, err := c.newOffer(seller.AccessToken, 1000, 10)
		if err != nil {
			return err
		}
		key := fmt.Sprintf("tekrar-%d", time.Now().UnixNano())
		var ids []string
		for range 3 {
			r, err := c.doKey(http.MethodPost, "/api/v1/orders", customer.AccessToken, key, orderBody(offerID, 2))
			if err != nil {
				return err
			}
			if err := r.expect(http.StatusCreated, ""); err != nil {
				return err
			}
			var o orderView
			if err := r.decode(&o); err != nil {
				return err
			}
			ids = append(ids, o.ID)
		}
		if ids[0] != ids[1] || ids[1] != ids[2] {
			return fmt.Errorf("aynı sipariş dönmeli: %v", ids)
		}
		if o, _ := c.offerStock(seller.AccessToken, offerID); o.Reserved != 2 {
			return fmt.Errorf("stok bir kez tutulmalı, reserved=%d", o.Reserved)
		}
		r, err := c.doKey(http.MethodPost, "/api/v1/orders", customer.AccessToken, key, orderBody(offerID, 3))
		if err != nil {
			return err
		}
		if err := r.expect(http.StatusUnprocessableEntity, "idempotency_key_reused"); err != nil {
			return err
		}
		_, _ = c.do(http.MethodPost, "/api/v1/orders/"+ids[0]+"/cancel", customer.AccessToken, nil)
		return nil
	}},
}

// otherSellerOffer demo satıcıdan farklı bir satıcının stoklu teklifini bulur (seed).
// Karşılaştırma satıcı kimliğiyle yapılır; verify'ın her çalıştırmada eklediği
// yeni ürünler sonucu etkilemez.
func (c *client) otherSellerOffer(token string, mine session) (string, error) {
	r, err := c.get("/api/v1/seller/offers?limit=1", mine.AccessToken)
	if err != nil {
		return "", err
	}
	var own struct {
		Items []offerView `json:"items"`
	}
	if err := r.decode(&own); err != nil || len(own.Items) == 0 {
		return "", fmt.Errorf("demo satıcının teklifi yok: %s", truncate(r.body))
	}
	mySeller := own.Items[0].SellerID

	cursor := ""
	for range 50 {
		r, err := c.get("/api/v1/products?limit=100"+cursor, token)
		if err != nil {
			return "", err
		}
		var list struct {
			Items []struct {
				ID         string `json:"id"`
				OfferCount int    `json:"offer_count"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if err := r.decode(&list); err != nil {
			return "", err
		}
		for _, it := range list.Items {
			r, err := c.get("/api/v1/products/"+it.ID, token)
			if err != nil {
				return "", err
			}
			var p product
			if err := r.decode(&p); err != nil {
				return "", err
			}
			for _, v := range p.Variants {
				for _, o := range v.Offers {
					if o.SellerID != mySeller && o.Available > 5 {
						return o.ID, nil
					}
				}
			}
		}
		if list.NextCursor == "" {
			break
		}
		cursor = "&cursor=" + list.NextCursor
	}
	return "", fmt.Errorf("başka satıcıya ait stoklu teklif bulunamadı (seed?)")
}
