// Package order çok satıcılı siparişi yönetir (F-3): müşteri tek sipariş
// verir, satıcı başına bir alt sipariş oluşur. Stok rezervasyonu inventory
// paketindedir; bu paket rezervasyonları siparişe bağlar.
//
// Bu dosya saf kurallardır: veritabanı ve HTTP bilmez.
package order

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"carsihub/internal/inventory"
	"carsihub/internal/platform/httpx"
)

const (
	MaxItems    = 50
	MaxQuantity = 100
)

var (
	ErrNotFound          = errors.New("sipariş bulunamadı")
	ErrNotCancellable    = errors.New("sipariş bu durumda iptal edilemez")
	ErrInsufficientStock = errors.New("sepetteki ürünler için yeterli stok yok")
	ErrEmpty             = errors.New("sipariş edilecek ürün yok")
)

type Status string

const (
	StatusPendingPayment Status = "pending_payment"
	StatusPaid           Status = "paid"
	StatusCancelled      Status = "cancelled"
	StatusExpired        Status = "expired"
)

type Order struct {
	ID              string              `json:"id"`
	CustomerID      string              `json:"customer_id"`
	Status          Status              `json:"status"`
	SubtotalKurus   int64               `json:"subtotal_kurus"`
	DiscountKurus   int64               `json:"discount_kurus"`
	TotalKurus      int64               `json:"total_kurus"`
	PaymentDeadline time.Time           `json:"payment_deadline"`
	CreatedAt       time.Time           `json:"created_at"`
	SubOrders       []SubOrder          `json:"sub_orders"`
	Unavailable     []inventory.Failure `json:"unavailable_items,omitempty"`
}

type SubOrder struct {
	ID            string `json:"id"`
	SellerID      string `json:"seller_id"`
	SellerName    string `json:"seller_name,omitempty"`
	Status        string `json:"status"`
	SubtotalKurus int64  `json:"subtotal_kurus"`
	DiscountKurus int64  `json:"discount_kurus"`
	TotalKurus    int64  `json:"total_kurus"`
	Items         []Line `json:"items"`
}

type Line struct {
	ID             string `json:"id"`
	OfferID        string `json:"offer_id"`
	ReservationID  string `json:"-"`
	Quantity       int    `json:"quantity"`
	UnitPriceKurus int64  `json:"unit_price_kurus"`
	LineTotalKurus int64  `json:"line_total_kurus"`
}

// ValidateItems sipariş girdisini doğrular; alan → hata eşlemesi döner.
func ValidateItems(items []inventory.Item) map[string]string {
	errs := map[string]string{}
	if len(items) == 0 {
		errs["items"] = "en az bir ürün gerekli"
		return errs
	}
	if len(items) > MaxItems {
		errs["items"] = fmt.Sprintf("en fazla %d kalem", MaxItems)
		return errs
	}
	for i, it := range items {
		key := fmt.Sprintf("items[%d]", i)
		switch {
		case strings.TrimSpace(it.OfferID) == "":
			errs[key+".offer_id"] = "zorunlu"
		case !httpx.IsUUID(it.OfferID):
			errs[key+".offer_id"] = "geçerli bir kimlik değil"
		}
		if it.Quantity < 1 || it.Quantity > MaxQuantity {
			errs[key+".quantity"] = fmt.Sprintf("1 ile %d arasında olmalı", MaxQuantity)
		}
	}
	for _, it := range inventory.MergeItems(items) {
		if it.Quantity > MaxQuantity {
			errs["items"] = fmt.Sprintf("aynı ürün için toplam adet en fazla %d", MaxQuantity)
		}
	}
	return errs
}

// Split rezervasyonları satıcıya göre alt siparişlere böler ve tutarları
// kuruş cinsinden hesaplar. Alt siparişler satıcı id'sine göre sıralıdır;
// çıktı aynı girdi için her zaman aynıdır.
func Split(reservations []inventory.Reservation) (subs []SubOrder, subtotal int64) {
	bySeller := map[string]*SubOrder{}
	var sellers []string
	for _, r := range reservations {
		s, ok := bySeller[r.SellerID]
		if !ok {
			s = &SubOrder{SellerID: r.SellerID, Status: string(StatusPendingPayment)}
			bySeller[r.SellerID] = s
			sellers = append(sellers, r.SellerID)
		}
		line := Line{
			OfferID:        r.OfferID,
			ReservationID:  r.ID,
			Quantity:       r.Quantity,
			UnitPriceKurus: r.UnitPriceKurus,
			LineTotalKurus: int64(r.Quantity) * r.UnitPriceKurus,
		}
		s.Items = append(s.Items, line)
		s.SubtotalKurus += line.LineTotalKurus
	}
	slices.Sort(sellers)
	for _, id := range sellers {
		s := bySeller[id]
		s.TotalKurus = s.SubtotalKurus - s.DiscountKurus
		subtotal += s.SubtotalKurus
		subs = append(subs, *s)
	}
	return subs, subtotal
}
