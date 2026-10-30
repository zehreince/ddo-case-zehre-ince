package pricing

import (
	"fmt"
	"slices"
)

// Kısmi iade kuralı: siparişte uygulanmış kuponlar (sipariş anındaki
// tanımlarıyla) kalan satırlara yeniden uygulanır. İade tutarı "iadeden önce
// ödenmesi gereken" ile "iadeden sonra ödenmesi gereken" arasındaki farktır.
//
// Böylece:
//   - indirim iade edilen satırla birlikte geri alınır (satırın indirimsiz fiyatı iade edilmez);
//   - kalan sepet minimum tutarın altına düşerse kupon düşer ve kaybedilen indirim
//     iade tutarından kesilir (clawback);
//   - satıcı bazında dağılım aynı yöntemle bulunur, kuruş kayması olmaz.
//
// Müşteriden ek ödeme istenmez: fark negatifse iade sıfırdır (bilinen sınırlama,
// bkz. ADR-009).

type RefundQuote struct {
	RefundKurus   int64                   `json:"refund_kurus"`
	BeforeKurus   int64                   `json:"paid_before_kurus"`
	AfterKurus    int64                   `json:"paid_after_kurus"`
	ClawbackKurus int64                   `json:"clawback_kurus"` // iade edilen kalemlerin ödenmiş payından kesilen
	LostCoupons   []string                `json:"lost_coupons"`
	BySeller      map[string]SellerRefund `json:"by_seller"`
}

// SellerRefund bir satıcının alt siparişindeki değişimdir (iade tutarı ve
// hangi tarafın karşıladığı indirimin ne kadar geri alındığı).
type SellerRefund struct {
	RefundKurus                 int64 `json:"refund_kurus"`
	SubtotalReturnedKurus       int64 `json:"subtotal_returned_kurus"`
	MarketplaceDiscountReversed int64 `json:"marketplace_discount_reversed_kurus"`
	SellerDiscountReversed      int64 `json:"seller_discount_reversed_kurus"`
}

// Refund daha önce iade edilenler (returned) düşüldükten sonra şimdi iade
// edilecek adetlerin (returning) iade tutarını hesaplar. applied siparişte
// fiilen uygulanmış kuponlardır.
func Refund(lines []Line, applied []Coupon, returned, returning map[string]int) (RefundQuote, error) {
	byID := map[string]Line{}
	for _, l := range lines {
		byID[l.ID] = l
	}
	for id, q := range returning {
		l, ok := byID[id]
		switch {
		case !ok:
			return RefundQuote{}, fmt.Errorf("satır siparişte yok: %s", id)
		case q <= 0:
			return RefundQuote{}, fmt.Errorf("iade adedi pozitif olmalı: %s", id)
		case returned[id]+q > l.Quantity:
			return RefundQuote{}, fmt.Errorf("satır %s için en fazla %d adet iade edilebilir", id, l.Quantity-returned[id])
		}
	}

	before := Calculate(keep(lines, returned, nil), applied)
	after := Calculate(keep(lines, returned, returning), applied)

	rq := RefundQuote{
		BeforeKurus: before.TotalKurus,
		AfterKurus:  after.TotalKurus,
		RefundKurus: max(0, before.TotalKurus-after.TotalKurus),
		LostCoupons: []string{},
		BySeller:    map[string]SellerRefund{},
	}

	// İade edilen kalemlerin "önceki" hesaptaki ödenmiş payı; iade bundan azsa fark clawback'tir.
	var returnedPaid int64
	for id, q := range returning {
		if l, ok := before.Line(id); ok {
			returnedPaid += mulDiv(l.TotalKurus, int64(q), int64(byID[id].Quantity-returned[id]))
		}
	}
	rq.ClawbackKurus = max(0, returnedPaid-rq.RefundKurus)

	for _, a := range before.Applied {
		if !slices.ContainsFunc(after.Applied, func(x Applied) bool { return x.Code == a.Code && x.DiscountKurus > 0 }) && a.DiscountKurus > 0 {
			rq.LostCoupons = append(rq.LostCoupons, a.Code)
		}
	}

	b, a := before.BySeller(), after.BySeller()
	for seller, bt := range b {
		at := a[seller]
		rq.BySeller[seller] = SellerRefund{
			RefundKurus:                 bt.TotalKurus - at.TotalKurus,
			SubtotalReturnedKurus:       bt.SubtotalKurus - at.SubtotalKurus,
			MarketplaceDiscountReversed: bt.MarketplaceDiscount - at.MarketplaceDiscount,
			SellerDiscountReversed:      bt.SellerDiscount - at.SellerDiscount,
		}
	}
	return rq, nil
}

// keep iade edilen adetleri düşülmüş satırları döner; adedi sıfıra inen satır çıkar.
func keep(lines []Line, maps ...map[string]int) []Line {
	var out []Line
	for _, l := range lines {
		for _, m := range maps {
			l.Quantity -= m[l.ID]
		}
		if l.Quantity > 0 {
			out = append(out, l)
		}
	}
	return out
}
