// Komut verify, ayağa kalkmış ve seed edilmiş sistem üzerinde uçtan uca
// kontrolleri çalıştırır ve sonunda PASS/FAIL tablosu basar. Bir kontrol bile
// düşerse çıkış kodu 1'dir.
//
// Her özellik eklendiğinde kendi kimliğiyle (F-1, T-1 ...) bir kontrol eklenir.
package main

import (
	"fmt"
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

func allChecks() []check {
	var all []check
	all = append(all, platformChecks...)
	all = append(all, authChecks...)
	all = append(all, catalogChecks...)
	all = append(all, inventoryChecks...)
	return all
}

func main() {
	base := os.Getenv("API_URL")
	if base == "" {
		base = "http://localhost:8080"
	}
	// Eşzamanlılık kontrolleri yüzlerce paralel istek atar; bağlantılar yeniden kullanılsın.
	transport := &http.Transport{MaxIdleConns: 256, MaxIdleConnsPerHost: 256, MaxConnsPerHost: 256}
	c := &client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 30 * time.Second, Transport: transport}}

	checks := allChecks()
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
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, "tablo yazılamadı:", err)
		os.Exit(2)
	}
	fmt.Printf("\n%d/%d kontrol geçti\n", len(checks)-failed, len(checks))
	if failed > 0 {
		os.Exit(1)
	}
}
