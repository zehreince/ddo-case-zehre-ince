// Package ledger çift taraflı finans defteridir (T-4).
//
// Her iş olayı bir işlemdir (Tx); işlemin borç ve alacak kayıtlarının toplamı
// eşittir. Kural üç yerde korunur: Validate (uygulama), ertelenmiş kısıt
// tetikleyicisi (commit anında veritabanı) ve append-only tetikleyici
// (geçmiş değiştirilemez, düzeltme ters kayıtla). Her işlemin bir idempotency
// anahtarı vardır: aynı olay iki kez gelse de deftere bir kez yazılır.
//
// Hesap planı:
//
//	platform:cash                     varlık   sağlayıcıda/bankada duran para
//	platform:customer_advances        borç     ödemesi alınmış, henüz teslim edilmemiş siparişler
//	seller:<id>:payable               borç     satıcıya ödenecek hakediş
//	platform:commission_revenue       gelir    komisyon
//	platform:shipping_revenue         gelir    satıcıdan kesilen kargo ücreti
//	platform:marketplace_discount     gider    pazaryerinin karşıladığı kupon indirimi
package ledger

import (
	"errors"
	"fmt"
	"time"
)

type AccountType string

const (
	Asset     AccountType = "asset"
	Liability AccountType = "liability"
	Revenue   AccountType = "revenue"
	Expense   AccountType = "expense"
)

const (
	Cash                = "platform:cash"
	CustomerAdvances    = "platform:customer_advances"
	CommissionRevenue   = "platform:commission_revenue"
	ShippingRevenue     = "platform:shipping_revenue"
	MarketplaceDiscount = "platform:marketplace_discount"
)

func SellerPayable(sellerID string) string { return "seller:" + sellerID + ":payable" }

// AccountTypeOf kod için hesap türünü döner.
func AccountTypeOf(code string) AccountType {
	switch code {
	case Cash:
		return Asset
	case CommissionRevenue, ShippingRevenue:
		return Revenue
	case MarketplaceDiscount:
		return Expense
	}
	return Liability // customer_advances ve seller:*:payable
}

type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

type Entry struct {
	Account   string
	Direction Direction
	Amount    int64
}

type Tx struct {
	Kind           string
	RefType, RefID string
	IdempotencyKey string
	Description    string
	OccurredAt     time.Time
	SellerID       string // satıcı hesabı açılırken satıcıya bağlamak için
	Entries        []Entry
}

// debit işaretli tutarla kayıt ekler: pozitif borç, negatif alacak, sıfır hiçbir şey.
// Böylece kurucular "bu hesap şu kadar azaldı/arttı" diye yazar, yön kendiliğinden doğru olur.
func (t *Tx) debit(account string, signed int64) {
	switch {
	case signed > 0:
		t.Entries = append(t.Entries, Entry{account, Debit, signed})
	case signed < 0:
		t.Entries = append(t.Entries, Entry{account, Credit, -signed})
	}
}

func (t *Tx) credit(account string, signed int64) { t.debit(account, -signed) }

var ErrUnbalanced = errors.New("defter işlemi dengesiz")

// Validate işlemin yazılabilir olduğunu denetler: en az iki kayıt, pozitif tutarlar, borç = alacak.
func (t Tx) Validate() error {
	if t.IdempotencyKey == "" || t.Kind == "" {
		return errors.New("işlem türü ve idempotency anahtarı zorunlu")
	}
	if len(t.Entries) < 2 {
		return fmt.Errorf("%w: en az iki kayıt gerekli", ErrUnbalanced)
	}
	var debit, credit int64
	for _, e := range t.Entries {
		if e.Amount <= 0 {
			return fmt.Errorf("kayıt tutarı pozitif olmalı: %+v", e)
		}
		switch e.Direction {
		case Debit:
			debit += e.Amount
		case Credit:
			credit += e.Amount
		default:
			return fmt.Errorf("geçersiz yön: %q", e.Direction)
		}
	}
	if debit != credit {
		return fmt.Errorf("%w: borç=%d alacak=%d", ErrUnbalanced, debit, credit)
	}
	return nil
}

// ── Kayıt kurucuları (saf) ───────────────────────────────────────────────

// PaymentCaptured: müşteriden para alındı, sipariş henüz teslim edilmedi.
//
//	Borç  platform:cash               ödenen
//	Alacak platform:customer_advances ödenen
func PaymentCaptured(orderID string, amount int64, at time.Time) Tx {
	t := Tx{Kind: "payment_captured", RefType: "order", RefID: orderID, IdempotencyKey: "capture:" + orderID,
		Description: "müşteri ödemesi", OccurredAt: at}
	t.debit(Cash, amount)
	t.credit(CustomerAdvances, amount)
	return t
}

// Accrual teslim edilen bir alt siparişin hakediş bileşenleridir.
type Accrual struct {
	SubOrderID          string
	SellerID            string
	PaidKurus           int64 // müşterinin bu alt sipariş için ödediği
	MarketplaceDiscount int64 // pazaryerinin karşıladığı kupon indirimi
	CommissionKurus     int64
	ShippingFeeKurus    int64
}

// Gross satıcının satış tutarıdır: müşterinin ödediği + pazaryerinin karşıladığı indirim
// (= ara toplam - satıcının kendi kupon indirimi).
func (a Accrual) Gross() int64 { return a.PaidKurus + a.MarketplaceDiscount }

// Net satıcıya ödenecek hakediştir: brüt - komisyon - kargo. Negatif olabilir
// (kargo ücreti satıştan büyükse); o zaman satıcı borçlu kalır ve sonraki ödemeden mahsup edilir.
func (a Accrual) Net() int64 { return a.Gross() - a.CommissionKurus - a.ShippingFeeKurus }

// SubOrderDelivered: teslimle gelir tanınır, müşteri avansı satıcıya ve pazaryerine dağılır.
//
//	Borç  platform:customer_advances   ödenen
//	Borç  platform:marketplace_discount pazaryeri indirimi
//	Alacak seller:<id>:payable          net
//	Alacak platform:commission_revenue  komisyon
//	Alacak platform:shipping_revenue    kargo
func SubOrderDelivered(a Accrual, at time.Time) Tx {
	t := Tx{Kind: "sub_order_delivered", RefType: "sub_order", RefID: a.SubOrderID, IdempotencyKey: "accrual:" + a.SubOrderID,
		Description: "teslim: hakediş tahakkuku", OccurredAt: at, SellerID: a.SellerID}
	t.debit(CustomerAdvances, a.PaidKurus)
	t.debit(MarketplaceDiscount, a.MarketplaceDiscount)
	t.credit(SellerPayable(a.SellerID), a.Net())
	t.credit(CommissionRevenue, a.CommissionKurus)
	t.credit(ShippingRevenue, a.ShippingFeeKurus)
	return t
}

// ReturnShare bir iadenin bir satıcıya düşen kısmıdır (işaretli: geri alınan pozitif).
type ReturnShare struct {
	SellerID                    string
	GrossReversed               int64 // satıcının satış tutarından geri alınan (ara toplam - satıcı indirimi)
	CommissionReversed          int64 // geri verilen komisyon
	MarketplaceDiscountReversed int64 // pazaryerinin geri aldığı indirim
}

// CashShare bu satıcının payına düşen müşteri iadesidir: satış tutarından geri
// alınan, pazaryerinin geri aldığı indirim kadar azalır.
func (r ReturnShare) CashShare() int64 { return r.GrossReversed - r.MarketplaceDiscountReversed }

// PayableReversed satıcının hakedişinden düşülen tutardır (iade kesintisi).
func (r ReturnShare) PayableReversed() int64 { return r.GrossReversed - r.CommissionReversed }

// Returned: teslim sonrası iade. Satıcı alacağı ve komisyon geri alınır, pazaryeri
// indirimi geri alınır, müşteriye iade ödenir. İade tutarı sıfıra kırpıldıysa
// (müşteriden ek ödeme istenmez) aradaki fark pazaryeri tarafından üstlenilir.
func Returned(returnID string, shares []ReturnShare, refundActual int64, at time.Time) Tx {
	t := Tx{Kind: "return", RefType: "return", RefID: returnID, IdempotencyKey: "return:" + returnID,
		Description: "iade: ters kayıt", OccurredAt: at}
	var model int64
	for _, s := range shares {
		t.debit(SellerPayable(s.SellerID), s.PayableReversed())
		t.debit(CommissionRevenue, s.CommissionReversed)
		t.credit(MarketplaceDiscount, s.MarketplaceDiscountReversed)
		model += s.CashShare()
	}
	t.credit(Cash, refundActual)
	t.debit(MarketplaceDiscount, refundActual-model)
	return t
}

// Payout: dönem kapanışında satıcıya ödeme.
//
//	Borç  seller:<id>:payable tutar
//	Alacak platform:cash      tutar
func Payout(periodID int64, sellerID string, amount int64, at time.Time) Tx {
	ref := fmt.Sprintf("%d:%s", periodID, sellerID)
	t := Tx{Kind: "payout", RefType: "payout", RefID: ref, IdempotencyKey: "payout:" + ref,
		Description: "dönem ödemesi", OccurredAt: at, SellerID: sellerID}
	t.debit(SellerPayable(sellerID), amount)
	t.credit(Cash, amount)
	return t
}
