package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type client struct {
	base string
	http *http.Client
}

type response struct {
	status int
	body   []byte
	header http.Header
}

// errorCode standart hata gövdesindeki kodu döner; gövde hata değilse boş.
func (r response) errorCode() string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(r.body, &e)
	return e.Error.Code
}

func (r response) decode(v any) error {
	if err := json.Unmarshal(r.body, v); err != nil {
		return fmt.Errorf("json çözülemedi (durum %d): %s", r.status, r.body)
	}
	return nil
}

// expect durum kodunu ve (verilmişse) hata kodunu kontrol eder.
func (r response) expect(status int, code string) error {
	if r.status != status || (code != "" && r.errorCode() != code) {
		return fmt.Errorf("beklenen %d %s, gelen %d %s", status, code, r.status, truncate(r.body))
	}
	return nil
}

func (c *client) do(method, path, token string, body any) (response, error) {
	return c.doKey(method, path, token, "", body)
}

// doKey isteği Idempotency-Key başlığıyla gönderir (boşsa başlıksız).
func (c *client) doKey(method, path, token, idemKey string, body any) (response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return response{}, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, c.base+path, rd)
	if err != nil {
		return response{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, body: b, header: resp.Header}, err
}

func (c *client) get(path, token string) (response, error) {
	return c.do(http.MethodGet, path, token, nil)
}

func truncate(b []byte) string {
	if len(b) > 160 {
		return string(b[:160]) + "…"
	}
	return string(b)
}
