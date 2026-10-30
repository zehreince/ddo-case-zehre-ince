package coupon

import (
	"testing"
	"time"

	"carsihub/internal/pricing"
)

func TestCreateInput_Validate(t *testing.T) {
	ok := CreateInput{Code: " yaz-24 ", Kind: "percent", Value: 1500, MaxDiscountKurus: 10000}
	c, errs := ok.Validate(pricing.ScopeMarketplace, "")
	if len(errs) != 0 || c.Code != "YAZ-24" || !c.Stackable {
		t.Fatalf("geçerli girdi: %+v %v", c, errs)
	}
	no := false
	if c, _ := (&CreateInput{Code: "ABC", Kind: "fixed", Value: 1, Stackable: &no}).Validate(pricing.ScopeSeller, "s1"); c.Stackable || c.SellerID != "s1" {
		t.Fatalf("satıcı kuponu: %+v", c)
	}

	start := time.Now()
	end := start.Add(-time.Hour)
	zero := 0
	bad := CreateInput{Code: "a", Kind: "percent", Value: 20000, StartsAt: &start, EndsAt: &end, MaxRedemptions: &zero, PerUserLimit: &zero}
	_, errs = bad.Validate(pricing.ScopeMarketplace, "")
	for _, k := range []string{"code", "coupon", "ends_at", "max_redemptions", "per_user_limit"} {
		if _, ok := errs[k]; !ok {
			t.Errorf("%s hatası bekleniyordu: %v", k, errs)
		}
	}
}

func TestNormalizeCode(t *testing.T) {
	if NormalizeCode("  pazar50 ") != "PAZAR50" {
		t.Fatal("kod büyük harfe çevrilmeli ve kırpılmalı")
	}
}
