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

	ReservationTTL     time.Duration // sepette stok tutma süresi
	PaymentTTL         time.Duration // siparişin ödeme beklediği süre
	PartialFulfillment string        // allow | reject
	SweepInterval      time.Duration // worker süpürme aralığı
	WorkerPort         int

	// Saga / dış bağımlılıklar (T-2)
	WebhookSecret      string        // sağlayıcı ve kargo ile paylaşılan HMAC anahtarı
	PaymentProviderURL string        // ödeme sağlayıcı (simülatör)
	CarrierURL         string        // kargo firması (simülatör)
	PublicBaseURL      string        // dış sistemlerin callback için API'ye ulaştığı adres
	ReconcileAfter     time.Duration // sonucu bu kadar gecikmiş ödemeyi sağlayıcıya sor
	OutboxPoll         time.Duration
	OutboxBatch        int
	OutboxMaxAttempts  int

	// Hakediş (T-4)
	PayoutDelay      time.Duration // teslimden sonra vade
	ShippingFeeKurus int64         // alt sipariş başına satıcıdan kesilen kargo ücreti
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

		ReservationTTL:     r.duration("RESERVATION_TTL", 15*time.Minute),
		PaymentTTL:         r.duration("ORDER_PAYMENT_TTL", 15*time.Minute),
		PartialFulfillment: r.str("PARTIAL_FULFILLMENT", "allow"),
		SweepInterval:      r.duration("SWEEP_INTERVAL", 5*time.Second),
		WorkerPort:         r.int("WORKER_PORT", 8081),

		WebhookSecret:      r.required("WEBHOOK_SECRET"),
		PaymentProviderURL: r.str("PAYMENT_PROVIDER_URL", "http://simulator:8090"),
		CarrierURL:         r.str("CARRIER_URL", "http://simulator:8090"),
		PublicBaseURL:      r.str("PUBLIC_BASE_URL", "http://api:8080"),
		ReconcileAfter:     r.duration("PAYMENT_RECONCILE_AFTER", 10*time.Second),
		OutboxPoll:         r.duration("OUTBOX_POLL_INTERVAL", 500*time.Millisecond),
		OutboxBatch:        r.int("OUTBOX_BATCH_SIZE", 100),
		OutboxMaxAttempts:  r.int("OUTBOX_MAX_ATTEMPTS", 10),

		PayoutDelay:      time.Duration(r.int("PAYOUT_DELAY_DAYS", 14)) * 24 * time.Hour,
		ShippingFeeKurus: int64(r.int("SHIPPING_FEE_KURUS", 2990)),
	}
	if cfg.WebhookSecret != "" && len(cfg.WebhookSecret) < minJWTSecretLen {
		r.errs = append(r.errs, fmt.Errorf("WEBHOOK_SECRET: en az %d karakter olmalı", minJWTSecretLen))
	}
	if cfg.OutboxBatch < 1 || cfg.OutboxMaxAttempts < 1 {
		r.errs = append(r.errs, errors.New("OUTBOX_BATCH_SIZE ve OUTBOX_MAX_ATTEMPTS en az 1 olmalı"))
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
	if cfg.PartialFulfillment != "allow" && cfg.PartialFulfillment != "reject" {
		r.errs = append(r.errs, fmt.Errorf("PARTIAL_FULFILLMENT: allow ya da reject olmalı: %q", cfg.PartialFulfillment))
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
