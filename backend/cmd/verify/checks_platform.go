package main

import (
	"fmt"
	"net/http"
	"strings"
)

var platformChecks = []check{
	{"HEALTH", "GET /health 200 ve veritabanı ok", func(c *client) error {
		r, err := c.get("/health", "")
		if err != nil {
			return err
		}
		var h struct{ Status, Database string }
		if err := r.decode(&h); err != nil {
			return err
		}
		if r.status != http.StatusOK || h.Database != "ok" {
			return fmt.Errorf("durum=%d database=%q", r.status, h.Database)
		}
		return nil
	}},
	{"API-ERR", "bilinmeyen uç standart hata gövdesi döner", func(c *client) error {
		r, err := c.get("/api/v1/olmayan-uc", "")
		if err != nil {
			return err
		}
		return r.expect(http.StatusNotFound, "not_found")
	}},
	{"METRICS", "GET /metrics Prometheus formatında", func(c *client) error {
		r, err := c.get("/metrics", "")
		if err != nil {
			return err
		}
		if r.status != http.StatusOK || !strings.Contains(string(r.body), "http_request_duration_seconds") {
			return fmt.Errorf("durum=%d, metrik bulunamadı", r.status)
		}
		return nil
	}},
}
