package config

import (
	"strings"
	"testing"
	"time"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func env(m map[string]string) LookupFunc {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// valid zorunlu alanları dolu bir ortam üretir; testler üzerine yazar.
func valid(extra map[string]string) LookupFunc {
	m := map[string]string{"DATABASE_URL": "postgres://x", "JWT_SECRET": testSecret}
	for k, v := range extra {
		m[k] = v
	}
	return env(m)
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load(valid(nil))
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if cfg.APIPort != 8080 || cfg.DBMaxConns != 50 || cfg.RequestTimeout != 10*time.Second {
		t.Fatalf("varsayılanlar yanlış: %+v", cfg)
	}
	if cfg.AccessTTL != 15*time.Minute || cfg.BcryptCost != 12 || cfg.CookieSecure {
		t.Fatalf("auth varsayılanları yanlış: %+v", cfg)
	}
	if len(cfg.CORSOrigins) != 0 {
		t.Fatalf("CORS boş olmalı: %v", cfg.CORSOrigins)
	}
}

func TestLoad_ParsesValues(t *testing.T) {
	cfg, err := Load(valid(map[string]string{
		"API_PORT":        "9090",
		"REQUEST_TIMEOUT": "3s",
		"CORS_ORIGINS":    " http://a , ,http://b ",
	}))
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if cfg.APIPort != 9090 || cfg.RequestTimeout != 3*time.Second {
		t.Fatalf("değerler okunmadı: %+v", cfg)
	}
	if strings.Join(cfg.CORSOrigins, "|") != "http://a|http://b" {
		t.Fatalf("CORS listesi yanlış: %v", cfg.CORSOrigins)
	}
}

func TestLoad_ReportsAllErrorsAtOnce(t *testing.T) {
	_, err := Load(env(map[string]string{
		"API_PORT":         "abc",
		"SHUTDOWN_TIMEOUT": "-1s",
	}))
	if err == nil {
		t.Fatal("hata bekleniyordu")
	}
	for _, want := range []string{"DATABASE_URL", "JWT_SECRET", "API_PORT", "SHUTDOWN_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("hata %q içermeli: %v", want, err)
		}
	}
}

func TestLoad_RejectsWeakAuthSettings(t *testing.T) {
	_, err := Load(valid(map[string]string{
		"JWT_SECRET":    "kisa",
		"BCRYPT_COST":   "3",
		"COOKIE_SECURE": "belki",
	}))
	if err == nil {
		t.Fatal("hata bekleniyordu")
	}
	for _, want := range []string{"JWT_SECRET", "BCRYPT_COST", "COOKIE_SECURE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("hata %q içermeli: %v", want, err)
		}
	}
}
