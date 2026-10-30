// Package pricing kupon ve fiyat motorudur (T-3). Saf hesaplamadır: veritabanı,
// saat ya da HTTP bilmez; aynı girdi her zaman aynı sonucu verir.
//
// Kurallar:
//   - Pazaryeri kuponunun indirimini pazaryeri, satıcı kuponununkini satıcı karşılar.
//     Satıcı kuponu yalnızca o satıcının satırlarına uygulanır.
//   - Kupon kategori listesiyle sınırlanabilir (boşsa tüm kategoriler).
//   - Minimum sepet, kuponun uygulanabildiği satırların İNDİRİMSİZ toplamına bakar.
//   - En fazla bir pazaryeri kuponu ve satıcı başına en fazla bir satıcı kuponu.
//     Fazlası girilme sırasına göre reddedilir.
//   - Birleştirilemeyen (stackable=false) kupon başka kuponla kullanılamaz: "o
//     kupon tek başına" ile "birleştirilebilenlerin hepsi" seçeneklerinden müşteriye
//     daha çok indirim sağlayan seçilir.
//   - Önce satıcı kuponları, sonra pazaryeri kuponu kalan tutara uygulanır.
//   - Yüzde indirim aşağı yuvarlanır; sabit indirim uygun tutarı aşamaz.
//   - Kupon indirimi uygun satırlara tutarlarıyla orantılı, en büyük kalan
//     yöntemiyle dağıtılır: satır payları toplamı indirime kuruşu kuruşuna eşittir.
package pricing

import (
	"cmp"
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"strings"
)

type Scope string

const (
	ScopeMarketplace Scope = "marketplace"
	ScopeSeller      Scope = "seller"
)

type Kind string

const (
	KindPercent Kind = "percent" // Value: baz puan (1000 = %10)
	KindFixed   Kind = "fixed"   // Value: kuruş
)

// Ret nedenleri: istemci bunlara göre mesaj gösterir.
const (
	ReasonMinBasket     = "min_basket"
	ReasonNoEligible    = "no_eligible_items"
	ReasonOnePerScope   = "one_per_scope"
	ReasonNotCombinable = "not_combinable"
	ReasonDuplicate     = "duplicate_code"
)

type Line struct {
	ID             string
	SellerID       string
	CategoryID     int64
	Quantity       int
	UnitPriceKurus int64
}

func (l Line) Subtotal() int64 { return int64(l.Quantity) * l.UnitPriceKurus }

type Coupon struct {
	ID               string  `json:"id"`
	Code             string  `json:"code"`
	Scope            Scope   `json:"scope"`
	SellerID         string  `json:"seller_id,omitempty"`
	Kind             Kind    `json:"kind"`
	Value            int64   `json:"value"`
	MaxDiscountKurus int64   `json:"max_discount_kurus,omitempty"` // yalnızca yüzde; 0 = sınırsız
	MinBasketKurus   int64   `json:"min_basket_kurus,omitempty"`
	CategoryIDs      []int64 `json:"category_ids,omitempty"`
	Stackable        bool    `json:"stackable"`
}

// Validate kupon tanımının kendi içinde tutarlı olduğunu denetler.
func (c Coupon) Validate() error {
	var errs []error
	switch c.Scope {
	case ScopeMarketplace:
		if c.SellerID != "" {
			errs = append(errs, errors.New("pazaryeri kuponu satıcıya bağlanamaz"))
		}
	case ScopeSeller:
		if c.SellerID == "" {
			errs = append(errs, errors.New("satıcı kuponu satıcı gerektirir"))
		}
	default:
		errs = append(errs, fmt.Errorf("geçersiz kapsam: %q", c.Scope))
	}
	switch c.Kind {
	case KindPercent:
		if c.Value <= 0 || c.Value > 10000 {
			errs = append(errs, errors.New("yüzde 1-10000 baz puan arasında olmalı"))
		}
	case KindFixed:
		if c.Value <= 0 {
			errs = append(errs, errors.New("sabit indirim pozitif olmalı"))
		}
		if c.MaxDiscountKurus != 0 {
			errs = append(errs, errors.New("üst sınır yalnızca yüzde indirimde anlamlı"))
		}
	default:
		errs = append(errs, fmt.Errorf("geçersiz tür: %q", c.Kind))
	}
	if c.MaxDiscountKurus < 0 || c.MinBasketKurus < 0 {
		errs = append(errs, errors.New("tutarlar negatif olamaz"))
	}
	return errors.Join(errs...)
}

type Allocation struct {
	LineID      string `json:"line_id"`
	CouponCode  string `json:"coupon_code"`
	FundedBy    Scope  `json:"funded_by"`
	AmountKurus int64  `json:"amount_kurus"`
}

type LineResult struct {
	LineID              string `json:"line_id"`
	SellerID            string `json:"seller_id"`
	SubtotalKurus       int64  `json:"subtotal_kurus"`
	MarketplaceDiscount int64  `json:"marketplace_discount_kurus"`
	SellerDiscount      int64  `json:"seller_discount_kurus"`
	TotalKurus          int64  `json:"total_kurus"`
}

func (l LineResult) DiscountKurus() int64 { return l.MarketplaceDiscount + l.SellerDiscount }

type Applied struct {
	CouponID      string `json:"coupon_id"`
	Code          string `json:"code"`
	Scope         Scope  `json:"scope"`
	DiscountKurus int64  `json:"discount_kurus"`
}

type Rejected struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

type Quote struct {
	Lines         []LineResult `json:"lines"`
	Allocations   []Allocation `json:"-"`
	Applied       []Applied    `json:"applied_coupons"`
	Rejected      []Rejected   `json:"rejected_coupons"`
	SubtotalKurus int64        `json:"subtotal_kurus"`
	DiscountKurus int64        `json:"discount_kurus"`
	TotalKurus    int64        `json:"total_kurus"`
}

type SellerTotal struct {
	SubtotalKurus       int64 `json:"subtotal_kurus"`
	MarketplaceDiscount int64 `json:"marketplace_discount_kurus"`
	SellerDiscount      int64 `json:"seller_discount_kurus"`
	TotalKurus          int64 `json:"total_kurus"`
}

// BySeller satır sonuçlarını satıcı bazında toplar (alt sipariş tutarları).
func (q Quote) BySeller() map[string]SellerTotal {
	out := map[string]SellerTotal{}
	for _, l := range q.Lines {
		t := out[l.SellerID]
		t.SubtotalKurus += l.SubtotalKurus
		t.MarketplaceDiscount += l.MarketplaceDiscount
		t.SellerDiscount += l.SellerDiscount
		t.TotalKurus += l.TotalKurus
		out[l.SellerID] = t
	}
	return out
}

// Line satır sonucunu kimliğiyle bulur.
func (q Quote) Line(id string) (LineResult, bool) {
	for _, l := range q.Lines {
		if l.LineID == id {
			return l, true
		}
	}
	return LineResult{}, false
}

// Calculate sepet satırlarına kuponları uygular. Kuponların sırası önceliktir
// (aynı kapsamda çakışırsa önce girilen kalır).
func Calculate(lines []Line, coupons []Coupon) Quote {
	lines = slices.Clone(lines)
	slices.SortFunc(lines, func(a, b Line) int { return cmp.Compare(a.ID, b.ID) })

	var rejected []Rejected
	reject := func(c Coupon, reason string) { rejected = append(rejected, Rejected{Code: c.Code, Reason: reason}) }

	// 1) Tek tek uygunluk ve kapsam çakışması
	var candidates []Coupon
	seenCode := map[string]bool{}
	marketplaceTaken := false
	sellerTaken := map[string]bool{}
	for _, c := range coupons {
		key := strings.ToUpper(c.Code)
		switch {
		case seenCode[key]:
			reject(c, ReasonDuplicate)
			continue
		case len(eligible(lines, c)) == 0:
			reject(c, ReasonNoEligible)
			continue
		case eligibleSubtotal(lines, c) < c.MinBasketKurus:
			reject(c, ReasonMinBasket)
			continue
		case c.Scope == ScopeMarketplace && marketplaceTaken,
			c.Scope == ScopeSeller && sellerTaken[c.SellerID]:
			reject(c, ReasonOnePerScope)
			continue
		}
		seenCode[key] = true
		if c.Scope == ScopeMarketplace {
			marketplaceTaken = true
		} else {
			sellerTaken[c.SellerID] = true
		}
		candidates = append(candidates, c)
	}

	// 2) Birlikte kullanım: birleştirilemeyen tek kupon ↔ birleştirilebilenlerin hepsi
	chosen := candidates
	if len(candidates) > 1 && slices.ContainsFunc(candidates, func(c Coupon) bool { return !c.Stackable }) {
		chosen = nil
		for _, c := range candidates {
			if c.Stackable {
				chosen = append(chosen, c)
			}
		}
		best := apply(lines, chosen).DiscountKurus
		for _, c := range candidates {
			if c.Stackable {
				continue
			}
			if d := apply(lines, []Coupon{c}).DiscountKurus; d > best {
				chosen, best = []Coupon{c}, d
			}
		}
		for _, c := range candidates {
			if !slices.ContainsFunc(chosen, func(x Coupon) bool { return x.Code == c.Code }) {
				reject(c, ReasonNotCombinable)
			}
		}
	}
	q := apply(lines, chosen)
	q.Rejected = rejected
	if q.Rejected == nil {
		q.Rejected = []Rejected{}
	}
	return q
}

// apply seçilmiş kuponları uygular: önce satıcı, sonra pazaryeri kuponları.
func apply(lines []Line, coupons []Coupon) Quote {
	remaining := make([]int64, len(lines))
	market := make([]int64, len(lines))
	seller := make([]int64, len(lines))
	for i, l := range lines {
		remaining[i] = l.Subtotal()
	}

	ordered := slices.Clone(coupons)
	slices.SortStableFunc(ordered, func(a, b Coupon) int {
		return cmp.Compare(scopeOrder(a.Scope), scopeOrder(b.Scope))
	})

	q := Quote{Applied: []Applied{}, Allocations: []Allocation{}}
	for _, c := range ordered {
		idx := eligible(lines, c)
		weights := make([]int64, len(idx))
		var base int64
		for j, i := range idx {
			weights[j] = remaining[i]
			base += remaining[i]
		}
		amount := discountAmount(c, base)
		shares := Allocate(amount, weights)
		for j, i := range idx {
			if shares[j] == 0 {
				continue
			}
			remaining[i] -= shares[j]
			if c.Scope == ScopeMarketplace {
				market[i] += shares[j]
			} else {
				seller[i] += shares[j]
			}
			q.Allocations = append(q.Allocations, Allocation{LineID: lines[i].ID, CouponCode: c.Code, FundedBy: c.Scope, AmountKurus: shares[j]})
		}
		q.Applied = append(q.Applied, Applied{CouponID: c.ID, Code: c.Code, Scope: c.Scope, DiscountKurus: amount})
	}

	q.Lines = make([]LineResult, len(lines))
	for i, l := range lines {
		r := LineResult{
			LineID: l.ID, SellerID: l.SellerID, SubtotalKurus: l.Subtotal(),
			MarketplaceDiscount: market[i], SellerDiscount: seller[i], TotalKurus: remaining[i],
		}
		q.Lines[i] = r
		q.SubtotalKurus += r.SubtotalKurus
		q.DiscountKurus += r.DiscountKurus()
		q.TotalKurus += r.TotalKurus
	}
	return q
}

func scopeOrder(s Scope) int {
	if s == ScopeSeller {
		return 0
	}
	return 1
}

// eligible kuponun uygulanabildiği satırların indislerini döner.
func eligible(lines []Line, c Coupon) []int {
	var idx []int
	for i, l := range lines {
		if c.Scope == ScopeSeller && l.SellerID != c.SellerID {
			continue
		}
		if len(c.CategoryIDs) > 0 && !slices.Contains(c.CategoryIDs, l.CategoryID) {
			continue
		}
		if l.Subtotal() > 0 {
			idx = append(idx, i)
		}
	}
	return idx
}

func eligibleSubtotal(lines []Line, c Coupon) int64 {
	var s int64
	for _, i := range eligible(lines, c) {
		s += lines[i].Subtotal()
	}
	return s
}

func discountAmount(c Coupon, base int64) int64 {
	var d int64
	switch c.Kind {
	case KindPercent:
		d = mulDiv(base, c.Value, 10000) // aşağı yuvarlama
		if c.MaxDiscountKurus > 0 {
			d = min(d, c.MaxDiscountKurus)
		}
	case KindFixed:
		d = c.Value
	}
	return max(0, min(d, base))
}

// Allocate amount'u ağırlıklarla orantılı ve toplamı tam amount olacak şekilde
// böler (en büyük kalan / Hamilton yöntemi). Hiçbir pay kendi ağırlığını aşmaz
// (amount ≤ toplam ağırlık olduğu sürece). Eşit kalanlarda büyük ağırlık, sonra
// önceki sıra kazanır; sonuç deterministiktir.
func Allocate(amount int64, weights []int64) []int64 {
	shares := make([]int64, len(weights))
	var total int64
	for _, w := range weights {
		total += w
	}
	if amount <= 0 || total <= 0 {
		return shares
	}
	amount = min(amount, total)

	type rem struct {
		i int
		r int64
	}
	rems := make([]rem, len(weights))
	var given int64
	for i, w := range weights {
		q, r := mulDivRem(amount, w, total)
		shares[i] = q
		given += q
		rems[i] = rem{i, r}
	}
	slices.SortStableFunc(rems, func(a, b rem) int {
		if c := cmp.Compare(b.r, a.r); c != 0 {
			return c
		}
		return cmp.Compare(weights[b.i], weights[a.i])
	})
	for k := int64(0); k < amount-given; k++ {
		shares[rems[k].i]++
	}
	return shares
}

// mulDiv a*b/c'yi 128-bit ara sonuçla hesaplar (kuruş × kuruş taşmasın).
func mulDiv(a, b, c int64) int64 {
	q, _ := mulDivRem(a, b, c)
	return q
}

// mulDivRem a*b'yi c'ye böler; bölüm ve kalanı döner.
// Önkoşul: a, b ≥ 0, c > 0 ve a*b/c int64'e sığar. Buradaki iki kullanımda
// (a ≤ c ile orantılı dağıtım, c = 10000 ile yüzde) bu her zaman sağlanır;
// bu da bits.Div64'ün gerektirdiği hi < c koşulunu garanti eder.
func mulDivRem(a, b, c int64) (int64, int64) {
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	q, r := bits.Div64(hi, lo, uint64(c))
	return int64(q), int64(r)
}
