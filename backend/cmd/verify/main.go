// Komut verify, ayağa kalkmış sistem üzerinde uçtan uca kontrolleri çalıştırır
// ve sonunda PASS/FAIL tablosu basar. Bir kontrol bile düşerse çıkış kodu 1'dir.
//
// Her özellik eklendiğinde buraya kendi kimliğiyle (F-1, T-1 ...) kontrol eklenir.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

type check struct {
	id   string
	name string
	run  func(*client) error
}

type client struct {
	base string
	http *http.Client
}

func (c *client) get(path string) (int, []byte, error) {
	resp, err := c.http.Get(c.base + path)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

var checks = []check{
	{"HEALTH", "GET /health 200 ve veritabanı ok", func(c *client) error {
		code, body, err := c.get("/health")
		if err != nil {
			return err
		}
		var h struct{ Status, Database string }
		if err := json.Unmarshal(body, &h); err != nil {
			return fmt.Errorf("json değil: %w", err)
		}
		if code != http.StatusOK || h.Database != "ok" {
			return fmt.Errorf("durum=%d database=%q", code, h.Database)
		}
		return nil
	}},
	{"API-ERR", "bilinmeyen uç standart hata gövdesi döner", func(c *client) error {
		code, body, err := c.get("/api/v1/olmayan-uc")
		if err != nil {
			return err
		}
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if err := json.Unmarshal(body, &e); err != nil || code != http.StatusNotFound || e.Error.Code == "" {
			return fmt.Errorf("durum=%d gövde=%s", code, body)
		}
		return nil
	}},
	{"METRICS", "GET /metrics Prometheus formatında", func(c *client) error {
		code, body, err := c.get("/metrics")
		if err != nil {
			return err
		}
		if code != http.StatusOK || !strings.Contains(string(body), "http_request_duration_seconds") {
			return fmt.Errorf("durum=%d, metrik bulunamadı", code)
		}
		return nil
	}},
}

func main() {
	base := os.Getenv("API_URL")
	if base == "" {
		base = "http://localhost:8080"
	}
	c := &client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 10 * time.Second}}

	failed := 0
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KİMLİK\tKONTROL\tSONUÇ\tAYRINTI")
	for _, ch := range checks {
		start := time.Now()
		err := ch.run(c)
		result, detail := "PASS", time.Since(start).Round(time.Millisecond).String()
		if err != nil {
			failed++
			result, detail = "FAIL", err.Error()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", ch.id, ch.name, result, detail)
	}
	tw.Flush()
	fmt.Printf("\n%d/%d kontrol geçti\n", len(checks)-failed, len(checks))
	if failed > 0 {
		os.Exit(1)
	}
}
