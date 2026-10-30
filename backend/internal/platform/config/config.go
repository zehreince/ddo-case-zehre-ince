// Package config tüm yapılandırmayı ortam değişkenlerinden okur.
// Zorunlu bir değer eksik ya da hatalıysa servis açılışta durur; sessizce
// varsayılana düşmez.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	APIPort         int
	LogLevel        string
	DatabaseURL     string
	DBMaxConns      int32
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	CORSOrigins     []string

	JWTSecret    string
	AccessTTL    time.Duration
	RefreshTTL   time.Duration
	BcryptCost   int
	CookieSecure bool
}

const minJWTSecretLen = 32

// LookupFunc os.LookupEnv imzasıdır; testlerde sahte ortam vermek için.
type LookupFunc func(key string) (string, bool)

func FromEnv() (Config, error) { return Load(os.LookupEnv) }

func Load(lookup LookupFunc) (Config, error) {
	r := reader{lookup: lookup}
	cfg := Config{
		APIPort:         r.int("API_PORT", 8080),
		LogLevel:        r.str("LOG_LEVEL", "info"),
		DatabaseURL:     r.required("DATABASE_URL"),
		DBMaxConns:      int32(r.int("DB_MAX_CONNS", 50)),
		ReadTimeout:     r.duration("HTTP_READ_TIMEOUT", 10*time.Second),
		WriteTimeout:    r.duration("HTTP_WRITE_TIMEOUT", 15*time.Second),
		RequestTimeout:  r.duration("REQUEST_TIMEOUT", 10*time.Second),
		ShutdownTimeout: r.duration("SHUTDOWN_TIMEOUT", 20*time.Second),
		CORSOrigins:     r.list("CORS_ORIGINS"),
		JWTSecret:       r.required("JWT_SECRET"),
		AccessTTL:       r.duration("JWT_ACCESS_TTL", 15*time.Minute),
		RefreshTTL:      r.duration("JWT_REFRESH_TTL", 7*24*time.Hour),
		BcryptCost:      r.int("BCRYPT_COST", 12),
		CookieSecure:    r.bool("COOKIE_SECURE", false),
	}
	if cfg.DBMaxConns < 1 {
		r.errs = append(r.errs, errors.New("DB_MAX_CONNS: en az 1 olmalı"))
	}
	if cfg.JWTSecret != "" && len(cfg.JWTSecret) < minJWTSecretLen {
		r.errs = append(r.errs, fmt.Errorf("JWT_SECRET: en az %d karakter olmalı", minJWTSecretLen))
	}
	if cfg.BcryptCost < 4 || cfg.BcryptCost > 31 {
		r.errs = append(r.errs, errors.New("BCRYPT_COST: 4 ile 31 arasında olmalı"))
	}
	if err := errors.Join(r.errs...); err != nil {
		return Config{}, fmt.Errorf("yapılandırma hatası: %w", err)
	}
	return cfg, nil
}

// reader hataları biriktirir; böylece eksik değerlerin hepsi tek seferde raporlanır.
type reader struct {
	lookup LookupFunc
	errs   []error
}

func (r *reader) str(key, def string) string {
	if v, ok := r.lookup(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func (r *reader) required(key string) string {
	v, ok := r.lookup(key)
	if !ok || strings.TrimSpace(v) == "" {
		r.errs = append(r.errs, fmt.Errorf("%s: zorunlu", key))
		return ""
	}
	return strings.TrimSpace(v)
}

func (r *reader) int(key string, def int) int {
	v := r.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: tam sayı değil: %q", key, v))
		return def
	}
	return n
}

func (r *reader) duration(key string, def time.Duration) time.Duration {
	v := r.str(key, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		r.errs = append(r.errs, fmt.Errorf("%s: geçerli bir süre değil: %q", key, v))
		return def
	}
	return d
}

func (r *reader) bool(key string, def bool) bool {
	v := r.str(key, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: true/false olmalı: %q", key, v))
		return def
	}
	return b
}

func (r *reader) list(key string) []string {
	var out []string
	for _, p := range strings.Split(r.str(key, ""), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
