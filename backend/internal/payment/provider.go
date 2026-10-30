package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"carsihub/internal/platform/webhook"
)

// Sağlayıcının bildirdiği ödeme durumları.
const (
	ProviderPending   = "pending"
	ProviderSucceeded = "succeeded"
	ProviderFailed    = "failed"
)

// ErrProviderNotFound sağlayıcı ödemeyi tanımıyor (hiç ulaşmamış ya da kaybetmiş).
var ErrProviderNotFound = errors.New("sağlayıcı ödemeyi tanımıyor")

// Provider ödeme sağlayıcının sistemimizin kullandığı yüzüdür. Tüm çağrılar
// ödeme kimliğiyle idempotenttir: aynı ödeme iki kez istenirse sağlayıcı
// ikinci kez tahsil etmez.
type Provider interface {
	Create(ctx context.Context, id string, amountKurus int64, callbackURL string) error
	Status(ctx context.Context, id string) (status, reason string, err error)
	Refund(ctx context.Context, id string) error
}

// HTTPProvider sağlayıcıya (simülatöre) imzalı HTTP çağrıları yapar.
type HTTPProvider struct {
	base   string
	secret []byte
	client *http.Client
}

func NewHTTPProvider(base string, secret []byte, timeout time.Duration) *HTTPProvider {
	return &HTTPProvider{base: base, secret: secret, client: &http.Client{Timeout: timeout}}
}

func (p *HTTPProvider) Create(ctx context.Context, id string, amountKurus int64, callbackURL string) error {
	_, err := p.call(ctx, http.MethodPost, "/payments", map[string]any{
		"id": id, "amount_kurus": amountKurus, "callback_url": callbackURL,
	})
	return err
}

func (p *HTTPProvider) Status(ctx context.Context, id string) (string, string, error) {
	b, err := p.call(ctx, http.MethodGet, "/payments/"+id, nil)
	if err != nil {
		return "", "", err
	}
	var r struct{ Status, Reason string }
	if err := json.Unmarshal(b, &r); err != nil {
		return "", "", fmt.Errorf("sağlayıcı yanıtı çözülemedi: %w", err)
	}
	return r.Status, r.Reason, nil
}

func (p *HTTPProvider) Refund(ctx context.Context, id string) error {
	_, err := p.call(ctx, http.MethodPost, "/payments/"+id+"/refund", map[string]any{})
	return err
}

func (p *HTTPProvider) call(ctx context.Context, method, path string, body any) ([]byte, error) {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	webhook.SetHeaders(req.Header, p.secret, raw, time.Now())
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ödeme sağlayıcısına ulaşılamadı: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrProviderNotFound
	case resp.StatusCode >= 300:
		return nil, fmt.Errorf("ödeme sağlayıcısı %d döndü: %s", resp.StatusCode, b)
	}
	return b, nil
}
