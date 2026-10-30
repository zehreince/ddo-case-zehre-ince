package main

import (
	"fmt"
	"net/http"
	"time"

	"carsihub/internal/auth"
)

type product struct {
	ID       string `json:"id"`
	Variants []struct {
		SKU    string `json:"sku"`
		Offers []struct {
			ID        string `json:"id"`
			SellerID  string `json:"seller_id"`
			Available int    `json:"available"`
		} `json:"offers"`
	} `json:"variants"`
}

type offerView struct {
	ID          string `json:"id"`
	SellerID    string `json:"seller_id"`
	StockOnHand int    `json:"stock_on_hand"`
	Reserved    int    `json:"stock_reserved"`
	Available   int    `json:"available"`
}

// newOffer satıcı olarak tek varyantlı yeni bir ürün oluşturur ve teklif kimliğini döner.
// Her çalıştırmada benzersiz SKU kullanılır; verify tekrar edilebilir.
func (c *client) newOffer(sellerTok string, priceKurus int64, stock int) (string, error) {
	cats, err := c.get("/api/v1/categories", sellerTok)
	if err != nil {
		return "", err
	}
	var cl struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	if err := cats.decode(&cl); err != nil || len(cl.Items) == 0 {
		return "", fmt.Errorf("kategori yok (seed çalıştı mı?): %s", truncate(cats.body))
	}
	r, err := c.do(http.MethodPost, "/api/v1/seller/products", sellerTok, map[string]any{
		"category_id": cl.Items[0].ID,
		"name":        "Verify ürünü",
		"variants": []map[string]any{{
			"sku": fmt.Sprintf("VRF-%d", time.Now().UnixNano()), "price_kurus": priceKurus, "stock": stock,
		}},
	})
	if err != nil {
		return "", err
	}
	if err := r.expect(http.StatusCreated, ""); err != nil {
		return "", err
	}
	var p product
	if err := r.decode(&p); err != nil {
		return "", err
	}
	return p.Variants[0].Offers[0].ID, nil
}

// offerStock satıcı tekliflerinden verileni bulur (stok ve rezerve miktar).
func (c *client) offerStock(sellerTok, offerID string) (offerView, error) {
	path := "/api/v1/seller/offers?limit=100"
	for range 20 {
		r, err := c.get(path, sellerTok)
		if err != nil {
			return offerView{}, err
		}
		var page struct {
			Items      []offerView `json:"items"`
			NextCursor string      `json:"next_cursor"`
		}
		if err := r.decode(&page); err != nil {
			return offerView{}, err
		}
		for _, o := range page.Items {
			if o.ID == offerID {
				return o, nil
			}
		}
		if page.NextCursor == "" {
			break
		}
		path = "/api/v1/seller/offers?limit=100&cursor=" + page.NextCursor
	}
	return offerView{}, fmt.Errorf("teklif %s satıcı listesinde yok", offerID)
}

var catalogChecks = []check{
	{"F-1", "satıcı varyantlı ürün ve stok ekler; katalogda satıcı bazlı görünür", func(c *client) error {
		seller, err := c.loginAs(auth.RoleSeller)
		if err != nil {
			return err
		}
		customer, err := c.loginAs(auth.RoleCustomer)
		if err != nil {
			return err
		}
		offerID, err := c.newOffer(seller.AccessToken, 2500, 7)
		if err != nil {
			return err
		}
		o, err := c.offerStock(seller.AccessToken, offerID)
		if err != nil {
			return err
		}
		if o.StockOnHand != 7 || o.Available != 7 {
			return fmt.Errorf("stok yanlış: %+v", o)
		}
		// Seed'deki çok satıcılı ürün: aynı varyantı iki satıcı kendi stoğuyla satar.
		multi, err := c.multiSellerVariant(customer.AccessToken)
		if err != nil {
			return err
		}
		if multi == "" {
			return fmt.Errorf("aynı varyantı birden fazla satıcının sattığı ürün bulunamadı")
		}
		// RBAC: müşteri ürün ekleyemez.
		r, err := c.do(http.MethodPost, "/api/v1/seller/products", customer.AccessToken, map[string]any{})
		if err != nil {
			return err
		}
		return r.expect(http.StatusForbidden, "forbidden")
	}},
	{"F-1b", "satıcı stoğu, tutulan miktarın altına indiremez", func(c *client) error {
		seller, err := c.loginAs(auth.RoleSeller)
		if err != nil {
			return err
		}
		customer, err := c.loginAs(auth.RoleCustomer)
		if err != nil {
			return err
		}
		offerID, err := c.newOffer(seller.AccessToken, 1000, 5)
		if err != nil {
			return err
		}
		r, err := c.do(http.MethodPost, "/api/v1/orders", customer.AccessToken, orderBody(offerID, 4))
		if err != nil {
			return err
		}
		if err := r.expect(http.StatusCreated, ""); err != nil {
			return err
		}
		r, err = c.do(http.MethodPut, "/api/v1/seller/offers/"+offerID+"/stock", seller.AccessToken, map[string]int{"stock_on_hand": 3})
		if err != nil {
			return err
		}
		if err := r.expect(http.StatusConflict, "below_reserved"); err != nil {
			return err
		}
		r, err = c.do(http.MethodPut, "/api/v1/seller/offers/"+offerID+"/stock", seller.AccessToken, map[string]int{"stock_on_hand": 10})
		if err != nil {
			return err
		}
		return r.expect(http.StatusOK, "")
	}},
}

// multiSellerVariant birden fazla satıcının teklif verdiği bir varyantın kimliğini döner.
// Tüm sayfalara bakar: verify her çalıştırmada yeni ürün eklediği için seed ürünü ilk sayfadan kayabilir.
func (c *client) multiSellerVariant(token string) (string, error) {
	cursor := ""
	for range 100 {
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
			if it.OfferCount < 2 {
				continue
			}
			r, err := c.get("/api/v1/products/"+it.ID, token)
			if err != nil {
				return "", err
			}
			var p product
			if err := r.decode(&p); err != nil {
				return "", err
			}
			for _, v := range p.Variants {
				if len(v.Offers) >= 2 && v.Offers[0].SellerID != v.Offers[1].SellerID {
					return v.SKU, nil
				}
			}
		}
		if list.NextCursor == "" {
			break
		}
		cursor = "&cursor=" + list.NextCursor
	}
	return "", nil
}

func orderBody(pairs ...any) map[string]any {
	var items []map[string]any
	for i := 0; i < len(pairs); i += 2 {
		items = append(items, map[string]any{"offer_id": pairs[i], "quantity": pairs[i+1]})
	}
	return map[string]any{"items": items}
}
