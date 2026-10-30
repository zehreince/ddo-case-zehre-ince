package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

func TestRole_Valid(t *testing.T) {
	for _, r := range allRoles {
		if !r.Valid() {
			t.Errorf("%s geçerli olmalı", r)
		}
	}
	for _, r := range []Role{"", "Admin", "root"} {
		if r.Valid() {
			t.Errorf("%q geçersiz olmalı", r)
		}
	}
}

func TestLoginInput_Normalize(t *testing.T) {
	cases := []struct {
		name       string
		in         LoginInput
		wantFields []string
		wantEmail  string
	}{
		{"geçerli, küçük harfe çevrilir", LoginInput{" Ayse@Ornek.COM ", "x"}, nil, "ayse@ornek.com"},
		{"boş alanlar", LoginInput{"", ""}, []string{"email", "password"}, ""},
		{"bozuk e-posta", LoginInput{"ayse@", "x"}, []string{"email"}, ""},
		{"isimli adres kabul edilmez", LoginInput{"Ayşe <a@b.co>", "x"}, []string{"email"}, ""},
		{"72 bayttan uzun parola", LoginInput{"a@b.co", strings.Repeat("ş", 37)}, []string{"password"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			errs := in.Normalize()
			if len(errs) != len(tc.wantFields) {
				t.Fatalf("hatalar=%v, beklenen alanlar=%v", errs, tc.wantFields)
			}
			for _, f := range tc.wantFields {
				if _, ok := errs[f]; !ok {
					t.Errorf("%s alanı hatalı olmalı: %v", f, errs)
				}
			}
			if tc.wantEmail != "" && in.Email != tc.wantEmail {
				t.Errorf("email=%q, beklenen %q", in.Email, tc.wantEmail)
			}
		})
	}
}

func TestPassword_HashAndCheck(t *testing.T) {
	h, err := HashPassword("Gizli123!", bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$2a$") || strings.Contains(h, "Gizli123!") {
		t.Fatalf("bcrypt özeti beklenmiyordu: %s", h)
	}
	if !CheckPassword(h, "Gizli123!") || CheckPassword(h, "gizli123!") {
		t.Fatal("parola doğrulaması yanlış")
	}
	if _, err := HashPassword(strings.Repeat("a", 73), bcrypt.MinCost); err == nil {
		t.Fatal("72 bayttan uzun parola reddedilmeli")
	}
}

const secret = "0123456789abcdef0123456789abcdef"

func TestTokens_RoundTrip(t *testing.T) {
	tk := NewTokens(secret, time.Minute)
	raw, exp, err := tk.IssueAccess(Principal{UserID: "u1", Role: RoleSeller})
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(exp) > time.Minute {
		t.Fatalf("süre yanlış: %v", exp)
	}
	p, err := tk.ParseAccess(raw)
	if err != nil || p.UserID != "u1" || p.Role != RoleSeller {
		t.Fatalf("p=%+v err=%v", p, err)
	}
}

func TestTokens_RejectsBadTokens(t *testing.T) {
	tk := NewTokens(secret, time.Minute)
	good, _, _ := tk.IssueAccess(Principal{UserID: "u1", Role: RoleCustomer})

	expired := NewTokens(secret, time.Minute)
	expired.now = func() time.Time { return time.Now().Add(-2 * time.Minute) }
	expiredTok, _, _ := expired.IssueAccess(Principal{UserID: "u1", Role: RoleCustomer})

	otherSecret, _, _ := NewTokens(strings.Repeat("x", 32), time.Minute).IssueAccess(Principal{UserID: "u1", Role: RoleAdmin})

	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, claims{Role: RoleAdmin, RegisteredClaims: jwt.RegisteredClaims{
		Issuer: issuer, Subject: "u1", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
	}}).SignedString(jwt.UnsafeAllowNoneSignatureType)

	unknownRole, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{Role: "root", RegisteredClaims: jwt.RegisteredClaims{
		Issuer: issuer, Subject: "u1", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
	}}).SignedString([]byte(secret))

	parts := strings.Split(good, ".")
	tampered := parts[0] + "." + parts[1] + "x." + parts[2]

	for name, raw := range map[string]string{
		"süresi dolmuş":          expiredTok,
		"başka anahtarla imzalı": otherSecret,
		"alg=none":               none,
		"bilinmeyen rol":         unknownRole,
		"değiştirilmiş gövde":    tampered,
		"çöp":                    "abc",
	} {
		if _, err := tk.ParseAccess(raw); err == nil {
			t.Errorf("%s: reddedilmeliydi", name)
		}
	}
}

func TestRefreshToken_IsRandomAndHashed(t *testing.T) {
	a, ha, err := NewRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := NewRefreshToken()
	if a == b || len(a) < 40 {
		t.Fatalf("token rastgele ve yeterince uzun olmalı: %q %q", a, b)
	}
	if len(ha) != 32 || string(HashRefreshToken(a)) != string(ha) {
		t.Fatal("özet SHA-256 olmalı ve tekrarlanabilir olmalı")
	}
}
