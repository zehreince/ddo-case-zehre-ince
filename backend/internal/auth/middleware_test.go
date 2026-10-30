package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBearer(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":   "abc",
		"bearer  abc ": "abc",
		"Basic abc":    "",
		"Bearer ":      "",
		"":             "",
	}
	for in, want := range cases {
		got, ok := bearer(in)
		if got != want || ok != (want != "") {
			t.Errorf("bearer(%q)=(%q,%v), beklenen %q", in, got, ok, want)
		}
	}
}

func TestAuthenticateAndRequireRole(t *testing.T) {
	tk := NewTokens(secret, time.Minute)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := PrincipalFrom(r.Context())
		w.Header().Set("X-User", p.UserID)
		w.WriteHeader(http.StatusOK)
	})
	h := Authenticate(tk)(RequireRole(RoleFinance, RoleAdmin)(ok))

	token := func(role Role) string {
		s, _, _ := tk.IssueAccess(Principal{UserID: "u-" + string(role), Role: role})
		return "Bearer " + s
	}
	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"başlık yok", "", http.StatusUnauthorized},
		{"geçersiz token", "Bearer bozuk", http.StatusUnauthorized},
		{"yetkisiz rol", token(RoleCustomer), http.StatusForbidden},
		{"izinli rol", token(RoleFinance), http.StatusOK},
		{"diğer izinli rol", token(RoleAdmin), http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != tc.want {
				t.Fatalf("durum %d, beklenen %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestRequireRole_WithoutAuthenticateIs401(t *testing.T) {
	rec := httptest.NewRecorder()
	RequireRole(RoleAdmin)(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("durum %d", rec.Code)
	}
}
