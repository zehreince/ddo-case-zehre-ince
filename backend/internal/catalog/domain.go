// Package catalog ürün, varyant ve satıcı tekliflerini (offer) yönetir (F-1).
// Ürün ve varyant pazaryerine aittir; aynı varyantı birden fazla satıcı kendi
// fiyatı ve stoğuyla satabilir.
//
// Bu dosya saf kurallardır: veritabanı ve HTTP bilmez.
package catalog

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	ErrNotFound      = errors.New("kayıt bulunamadı")
	ErrNotSeller     = errors.New("bu kullanıcıya bağlı bir satıcı yok")
	ErrBelowReserved = errors.New("stok, sepetlerde ve ödeme bekleyen siparişlerde tutulan miktarın altına indirilemez")
	ErrDuplicateSKU  = errors.New("bu SKU zaten kullanılıyor")
	ErrBadCategory   = errors.New("kategori bulunamadı")
)

const (
	MaxVariants   = 20
	MaxStock      = 1_000_000
	MaxPriceKurus = 10_000_000_00 // 10 milyon TL
)

var skuPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,63}$`)

type CreateProductInput struct {
	CategoryID  int64             `json:"category_id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Attributes  map[string]string `json:"attributes"`
	Variants    []VariantInput    `json:"variants"`
}

type VariantInput struct {
	SKU        string            `json:"sku"`
	Attributes map[string]string `json:"attributes"`
	PriceKurus int64             `json:"price_kurus"`
	Stock      int               `json:"stock"`
}

// Normalize boşlukları kırpar ve alanları doğrular; alan → hata eşlemesi döner.
func (in *CreateProductInput) Normalize() map[string]string {
	errs := map[string]string{}
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	if in.CategoryID <= 0 {
		errs["category_id"] = "zorunlu"
	}
	if n := len([]rune(in.Name)); n == 0 || n > 300 {
		errs["name"] = "1-300 karakter olmalı"
	}
	if len(in.Variants) == 0 || len(in.Variants) > MaxVariants {
		errs["variants"] = fmt.Sprintf("1-%d varyant olmalı", MaxVariants)
	}
	seen := map[string]bool{}
	for i := range in.Variants {
		v := &in.Variants[i]
		key := fmt.Sprintf("variants[%d]", i)
		v.SKU = strings.TrimSpace(v.SKU)
		switch {
		case !skuPattern.MatchString(v.SKU):
			errs[key+".sku"] = "2-64 karakter; harf, rakam, . _ -"
		case seen[strings.ToUpper(v.SKU)]:
			errs[key+".sku"] = "aynı üründe tekrar ediyor"
		}
		seen[strings.ToUpper(v.SKU)] = true
		if v.PriceKurus <= 0 || v.PriceKurus > MaxPriceKurus {
			errs[key+".price_kurus"] = "pozitif olmalı (kuruş cinsinden)"
		}
		if v.Stock < 0 || v.Stock > MaxStock {
			errs[key+".stock"] = fmt.Sprintf("0-%d arasında olmalı", MaxStock)
		}
	}
	return errs
}

type ProductSummary struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	CategoryID    int64     `json:"category_id"`
	CategoryName  string    `json:"category_name"`
	MinPriceKurus *int64    `json:"min_price_kurus"`
	Available     int       `json:"available"`
	OfferCount    int       `json:"offer_count"`
	CreatedAt     time.Time `json:"created_at"`
}

type Product struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	CategoryID   int64             `json:"category_id"`
	CategoryName string            `json:"category_name"`
	Attributes   map[string]string `json:"attributes"`
	Variants     []Variant         `json:"variants"`
}

type Variant struct {
	ID         string            `json:"id"`
	SKU        string            `json:"sku"`
	Attributes map[string]string `json:"attributes"`
	Offers     []Offer           `json:"offers"`
}

type Offer struct {
	ID          string    `json:"id"`
	SellerID    string    `json:"seller_id"`
	SellerName  string    `json:"seller_name"`
	VariantID   string    `json:"variant_id"`
	ProductName string    `json:"product_name,omitempty"`
	SKU         string    `json:"sku,omitempty"`
	PriceKurus  int64     `json:"price_kurus"`
	StockOnHand int       `json:"stock_on_hand"`
	Reserved    int       `json:"stock_reserved"`
	Available   int       `json:"available"`
	CreatedAt   time.Time `json:"created_at"`
}

type Category struct {
	ID       int64  `json:"id"`
	ParentID *int64 `json:"parent_id"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
}
