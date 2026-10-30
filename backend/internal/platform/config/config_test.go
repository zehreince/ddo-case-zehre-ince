package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) LookupFunc {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{"DATABASE_URL": "postgres://x"}))
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if cfg.APIPort != 8080 || cfg.DBMaxConns != 50 || cfg.RequestTimeout != 10*time.Second {
		t.Fatalf("varsayılanlar yanlış: %+v", cfg)
	}
	if len(cfg.CORSOrigins) != 0 {
		t.Fatalf("CORS boş olmalı: %v", cfg.CORSOrigins)
	}
}

func TestLoad_ParsesValues(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL":    "postgres://x",
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
	for _, want := range []string{"DATABASE_URL", "API_PORT", "SHUTDOWN_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("hata %q içermeli: %v", want, err)
		}
	}
}
