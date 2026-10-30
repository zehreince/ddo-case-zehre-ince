package main

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"carsihub/internal/auth"
)

type couponOrder struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	SubtotalKurus int64  `json:"subtotal_kurus"`
	DiscountKurus int64  `json:"discount_kurus"`
	TotalKurus    int64  `json:"total_kurus"`
	SubOrders     []struct {
		SellerID                 string `json:"seller_id"`
		DiscountKurus            int64  `json:"discount_kurus"`
		TotalKurus               int64  `json:"total_kurus"`
		MarketplaceDiscountKurus int64  `json:"marketplace_discount_kurus"`
		SellerDiscountKurus      int64  `json:"seller_discount_kurus"`
		Items                    []struct {
			ID                       string `json:"id"`
			OfferID                  string `json:"offer_id"`
			MarketplaceDiscountKurus int64  `json:"marketplace_discount_kurus"`
			SellerDiscountKurus      int64  `json:"seller_discount_kurus"`
			PaidKurus                int64  `json:"paid_kurus"`
		} `json:"items"`
	} `json:"sub_orders"`
	AppliedCoupons []struct {
		Code          string `json:"code"`
		DiscountKurus int64  `json:"discount_kurus"`
	} `json:"applied_coupons"`
	RejectedCoupons []struct{ Code, Reason string } `json:"rejected_coupons"`
}

// consistent alt sipariş ve satır tutarlarının sipariş toplamına kuruşu kuruşuna eşit olduğunu denetler.
func (o couponOrder) consistent() error {
	var subTotal, subDisc, itemPaid, itemDisc, couponDisc int64
	for _, s := range o.SubOrders {
		subTotal += s.TotalKurus
		subDisc += s.DiscountKurus
		if s.DiscountKurus != s.MarketplaceDiscountKurus+s.SellerDiscountKurus {
			return fmt.Errorf("alt sipariş indirim kırılımı tutmuyor: %+v", s)
		}
		for _, it := range s.Items {
			itemPaid += it.PaidKurus
			itemDisc += it.MarketplaceDiscountKurus + it.SellerDiscountKurus
		}
	}
	for _, c := range o.AppliedCoupons {
		couponDisc += c.DiscountKurus
	}
	if subTotal != o.TotalKurus || itemPaid != o.TotalKurus || subDisc != o.DiscountKurus || itemDisc != o.DiscountKurus || couponDisc != o.DiscountKurus {
		return fmt.Errorf("kuruş kayması: toplam=%d alt=%d satır=%d | indirim=%d alt=%d satır=%d kupon=%d",
			o.TotalKurus, subTotal, itemPaid, o.DiscountKurus, subDisc, itemDisc, couponDisc)
	}
	return nil
}

// newCoupon yönetici (pazaryeri) ya da satıcı olarak benzersiz kodlu kupon açar.
func (c *client) newCoupon(token, prefix string, body map[string]any) (string, error) {
	code := fmt.Sprintf("%s%d", prefix, time.Now().UnixNano()%1_000_000_000)
	body["code"] = code
	r, err := c.do(http.MethodPost, "/api/v1/coupons", token, body)
	if err != nil {
		return "", err
	}
	return code, r.expect(http.StatusCreated, "")
}

func (c *client) placeCoupon(token string, codes []string, pairs ...any) (couponOrder, error) {
	body := orderBody(pairs...)
	body["coupon_codes"] = codes
	r, err := c.doKey(http.MethodPost, "/api/v1/orders", token, fmt.Sprintf("kupon-%d", time.Now().UnixNano()), body)
	if err != nil {
		return couponOrder{}, err
	}
	var o couponOrder
	if err := r.expect(http.StatusCreated, ""); err != nil {
		return o, err
	}
	return o, r.decode(&o)
}

var couponChecks = []check{
	{"T-3a", "pazaryeri + satıcı kuponu: indirim satıcılara orantılı, kuruş kayması yok", func(c *client) error {
		admin, err := c.loginAs(auth.RoleAdmin)
		if err != nil {
			return err
		}
		seller, customer, err := sessions(c)
		if err != nil {
			return err
		}
		// Üç eşit satır (33,33 TL) üzerine 10,00 TL: 3,34 + 3,33 + 3,33 olmalı.
		var offers []string
		for range 3 {
			id, err := c.newOffer(seller.AccessToken, 3333, 10)
			if err != nil {
				return err
			}
			offers = append(offers, id)
		}
		other, err := c.otherSellerOffer(customer.AccessToken, seller)
		if err != nil {
			return err
		}
		market, err := c.newCoupon(admin.AccessToken, "VP", map[string]any{"kind": "fixed", "value": 1000})
		if err != nil {
			return err
		}
		mine, err := c.newCoupon(seller.AccessToken, "VS", map[string]any{"kind": "percent", "value": 1000})
		if err != nil {
			return err
		}
		o, err := c.placeCoupon(customer.AccessToken, []string{market, mine}, offers[0], 1, offers[1], 1, offers[2], 1, other, 1)
		if err != nil {
			return err
		}
		if len(o.AppliedCoupons) != 2 || len(o.SubOrders) != 2 {
			return fmt.Errorf("iki kupon ve iki alt sipariş bekleniyordu: %+v", o)
		}
		if err := o.consistent(); err != nil {
			return err
		}
		for _, s := range o.SubOrders {
			if s.SellerDiscountKurus > 0 && s.SellerID != o.SubOrders[0].SellerID && s.SellerID != o.SubOrders[1].SellerID {
				return fmt.Errorf("satıcı kuponu başka satıcıya uygulanmış")
			}
		}
		_, _ = c.do(http.MethodPost, "/api/v1/orders/"+o.ID+"/cancel", customer.AccessToken, nil)
		return logf("indirim %d kuruş, %d satıcıya dağıldı", o.DiscountKurus, len(o.SubOrders))
	}},
	{"T-3b", "minimum sepet, kategori ve birlikte kullanım kuralları", func(c *client) error {
		admin, err := c.loginAs(auth.RoleAdmin)
		if err != nil {
			return err
		}
		seller, customer, err := sessions(c)
		if err != nil {
			return err
		}
		offer, err := c.newOffer(seller.AccessToken, 10000, 10)
		if err != nil {
			return err
		}
		minHigh, err := c.newCoupon(admin.AccessToken, "VMIN", map[string]any{"kind": "fixed", "value": 500, "min_basket_kurus": 50000})
		if err != nil {
			return err
		}
		wrongCat, err := c.newCoupon(admin.AccessToken, "VCAT", map[string]any{"kind": "fixed", "value": 500, "category_ids": []int{999999}})
		if err != nil {
			return err
		}
		o, err := c.placeCoupon(customer.AccessToken, []string{minHigh, wrongCat, "YOKBOYLE"}, offer, 1)
		if err != nil {
			return err
		}
		reasons := map[string]string{}
		for _, r := range o.RejectedCoupons {
			reasons[r.Code] = r.Reason
		}
		if reasons[minHigh] != "min_basket" || reasons[wrongCat] != "no_eligible_items" || reasons["YOKBOYLE"] != "not_found" || o.DiscountKurus != 0 {
			return fmt.Errorf("ret nedenleri: %v", reasons)
		}
		_, _ = c.do(http.MethodPost, "/api/v1/orders/"+o.ID+"/cancel", customer.AccessToken, nil)

		// Birleştirilemeyen kupon daha avantajlıysa tek başına seçilir, diğeri reddedilir.
		alone, err := c.newCoupon(admin.AccessToken, "VTEK", map[string]any{"kind": "fixed", "value": 3000, "stackable": false})
		if err != nil {
			return err
		}
		small, err := c.newCoupon(seller.AccessToken, "VKUC", map[string]any{"kind": "fixed", "value": 1000})
		if err != nil {
			return err
		}
		o, err = c.placeCoupon(customer.AccessToken, []string{small, alone}, offer, 1)
		if err != nil {
			return err
		}
		if o.DiscountKurus != 3000 || len(o.RejectedCoupons) != 1 || o.RejectedCoupons[0].Reason != "not_combinable" {
			return fmt.Errorf("birlikte kullanım: indirim=%d ret=%v", o.DiscountKurus, o.RejectedCoupons)
		}
		_, _ = c.do(http.MethodPost, "/api/v1/orders/"+o.ID+"/cancel", customer.AccessToken, nil)
		return nil
	}},
	{"T-3c", "kullanım limiti eşzamanlı siparişte aşılmaz", func(c *client) error {
		admin, err := c.loginAs(auth.RoleAdmin)
		if err != nil {
			return err
		}
		seller, customer, err := sessions(c)
		if err != nil {
			return err
		}
		offer, err := c.newOffer(seller.AccessToken, priceHang, 100)
		if err != nil {
			return err
		}
		code, err := c.newCoupon(admin.AccessToken, "VLIM", map[string]any{"kind": "fixed", "value": 100, "max_redemptions": 3})
		if err != nil {
			return err
		}
		var (
			wg         sync.WaitGroup
			mu         sync.Mutex
			discounted int
			ids        []string
		)
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				o, err := c.placeCoupon(customer.AccessToken, []string{code}, offer, 1)
				if err != nil {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				ids = append(ids, o.ID)
				if o.DiscountKurus == 100 {
					discounted++
				}
			}()
		}
		wg.Wait()
		for _, id := range ids {
			_, _ = c.do(http.MethodPost, "/api/v1/orders/"+id+"/cancel", customer.AccessToken, nil)
		}
		if discounted != 3 || len(ids) != 20 {
			return fmt.Errorf("20 siparişten tam 3'ü indirimli olmalı: indirimli=%d sipariş=%d", discounted, len(ids))
		}
		return nil
	}},
	{"T-3d", "kısmi iade: indirim geri hesaplanır, min. sepet altında kupon geri alınır", func(c *client) error {
		admin, err := c.loginAs(auth.RoleAdmin)
		if err != nil {
			return err
		}
		seller, customer, err := sessions(c)
		if err != nil {
			return err
		}
		a, err := c.newOffer(seller.AccessToken, 20000, 10)
		if err != nil {
			return err
		}
		b, err := c.newOffer(seller.AccessToken, 10000, 10)
		if err != nil {
			return err
		}
		// 300 - 49,95 = 250,05 TL (,05 → simülatörde ödeme kesin başarılı)
		code, err := c.newCoupon(admin.AccessToken, "VIADE", map[string]any{"kind": "fixed", "value": 4995, "min_basket_kurus": 25000})
		if err != nil {
			return err
		}
		o, err := c.placeCoupon(customer.AccessToken, []string{code}, a, 1, b, 1)
		if err != nil {
			return err
		}
		if _, _, err := c.waitOrder(customer.AccessToken, o.ID, 30*time.Second, func(s sagaOrder) bool { return s.Status == "paid" }); err != nil {
			return err
		}
		var itemB string
		for _, s := range o.SubOrders {
			for _, it := range s.Items {
				if it.OfferID == b {
					itemB = it.ID
				}
			}
		}
		r, err := c.do(http.MethodPost, "/api/v1/orders/"+o.ID+"/refund-quote", customer.AccessToken,
			map[string]any{"items": []map[string]any{{"order_item_id": itemB, "quantity": 1}}})
		if err != nil {
			return err
		}
		var rq struct {
			RefundKurus   int64    `json:"refund_kurus"`
			ClawbackKurus int64    `json:"clawback_kurus"`
			LostCoupons   []string `json:"lost_coupons"`
		}
		if err := r.expect(http.StatusOK, ""); err != nil {
			return err
		}
		if err := r.decode(&rq); err != nil {
			return err
		}
		// Kalan 200 TL < 250 TL → kupon düşer: iade 250,05 - 200,00 = 50,05 TL.
		if rq.RefundKurus != 5005 || len(rq.LostCoupons) != 1 || rq.ClawbackKurus <= 0 {
			return fmt.Errorf("iade önizlemesi: %+v", rq)
		}
		return logf("iade %d kuruş, kesinti %d kuruş", rq.RefundKurus, rq.ClawbackKurus)
	}},
}
