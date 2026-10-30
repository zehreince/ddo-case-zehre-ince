package auth

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/platform/httpx"
)

func newTestServer(t *testing.T) (*httptest.Server, *pgxpool.Pool) {
	s, pool := newTestService(t)
	r := chi.NewRouter()
	r.Route("/api/v1", NewHandler(s, s.tokens, slog.New(slog.NewTextHandler(io.Discard, nil)), false).Mount)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, pool
}

func do(t *testing.T, method, url, token, body string, cookies ...*http.Cookie) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e httpx.ErrorBody
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("hata gövdesi değil: %s", body)
	}
	return e.Error.Code
}

func TestHTTP_LoginFlow(t *testing.T) {
	srv, pool := newTestServer(t)
	createUser(t, pool, "musteri@b.co", RoleCustomer)
	createUser(t, pool, "admin@b.co", RoleAdmin)
	api := srv.URL + "/api/v1"

	// Doğrulama hatası: alan bazlı 422
	resp, body := do(t, "POST", api+"/auth/login", "", `{"email":"bozuk","password":""}`)
	if resp.StatusCode != http.StatusUnprocessableEntity || errorCode(t, body) != httpx.CodeValidation ||
		!strings.Contains(string(body), `"email"`) || !strings.Contains(string(body), `"password"`) {
		t.Fatalf("422 bekleniyordu: %d %s", resp.StatusCode, body)
	}
	// Bilinmeyen alan reddedilir
	if resp, _ := do(t, "POST", api+"/auth/login", "", `{"email":"a@b.co","password":"x","role":"admin"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bilinmeyen alan 400 olmalı: %d", resp.StatusCode)
	}
	// Yanlış parola
	resp, body = do(t, "POST", api+"/auth/login", "", `{"email":"musteri@b.co","password":"yanlis"}`)
	if resp.StatusCode != http.StatusUnauthorized || errorCode(t, body) != "invalid_credentials" {
		t.Fatalf("401 invalid_credentials bekleniyordu: %d %s", resp.StatusCode, body)
	}

	// Başarılı giriş: gövde + httpOnly çerez
	resp, body = do(t, "POST", api+"/auth/login", "", `{"email":"MUSTERI@b.co","password":"`+testPassword+`"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("giriş: %d %s", resp.StatusCode, body)
	}
	var sess Session
	_ = json.Unmarshal(body, &sess)
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == refreshCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Value != sess.RefreshToken {
		t.Fatalf("refresh çerezi yanlış: %+v", cookie)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("token yanıtı önbelleğe alınmamalı")
	}

	// /me
	if resp, _ := do(t, "GET", api+"/auth/me", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token'sız /me 401 olmalı: %d", resp.StatusCode)
	}
	resp, body = do(t, "GET", api+"/auth/me", sess.AccessToken, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "musteri@b.co") {
		t.Fatalf("/me: %d %s", resp.StatusCode, body)
	}

	// RBAC: müşteri yönetici ucuna giremez
	resp, body = do(t, "GET", api+"/admin/users", sess.AccessToken, "")
	if resp.StatusCode != http.StatusForbidden || errorCode(t, body) != httpx.CodeForbidden {
		t.Fatalf("müşteri için 403 bekleniyordu: %d %s", resp.StatusCode, body)
	}

	// Çerezle yenileme (tarayıcı yolu)
	resp, body = do(t, "POST", api+"/auth/refresh", "", "", cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("çerezle yenileme: %d %s", resp.StatusCode, body)
	}
	// Eski çerez artık yeniden kullanım sayılır
	resp, body = do(t, "POST", api+"/auth/refresh", "", "", cookie)
	if resp.StatusCode != http.StatusUnauthorized || errorCode(t, body) != "token_reused" {
		t.Fatalf("yeniden kullanım 401 token_reused olmalı: %d %s", resp.StatusCode, body)
	}

	// Yönetici listeyi görebilir; sayfalama parametreleri doğrulanır
	_, body = do(t, "POST", api+"/auth/login", "", `{"email":"admin@b.co","password":"`+testPassword+`"}`)
	var admin Session
	_ = json.Unmarshal(body, &admin)
	resp, body = do(t, "GET", api+"/admin/users?limit=1", admin.AccessToken, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"next_cursor"`) {
		t.Fatalf("yönetici listesi: %d %s", resp.StatusCode, body)
	}
	for _, q := range []string{"?limit=0", "?limit=abc", "?cursor=bozuk"} {
		if resp, _ := do(t, "GET", api+"/admin/users"+q, admin.AccessToken, ""); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s için 422 bekleniyordu: %d", q, resp.StatusCode)
		}
	}

	// Çıkış: 204, çerez silinir, token artık geçersiz
	resp, _ = do(t, "POST", api+"/auth/logout", "", `{"refresh_token":"`+admin.RefreshToken+`"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("çıkış: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "POST", api+"/auth/refresh", "", `{"refresh_token":"`+admin.RefreshToken+`"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("çıkış sonrası yenileme 401 olmalı: %d", resp.StatusCode)
	}
}
