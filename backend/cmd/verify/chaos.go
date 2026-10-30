package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"carsihub/internal/auth"
)

// chaos kaos betiğinin API'ye dokunan adımlarıdır. Konteynerleri durdurup
// başlatmak betiğin (scripts/chaos.sh) işidir; buradaki adımlar yalnızca
// gözlemler. Çıktının stdout kısmı betik tarafından okunur, açıklamalar stderr'e.
//
//	chaos place N           N sipariş verir, kimliklerini stdout'a yazar
//	chaos status ID...      siparişlerin ve outbox'ın durumunu yazar
//	chaos await SN ID...    siparişlerin hepsi ödenene kadar en fazla SN saniye bekler
func chaos(c *client, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "kullanım: verify chaos place|status|await ...")
		return 2
	}
	seller, customer, err := sessions(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "giriş:", err)
		return 1
	}
	switch args[0] {
	case "place":
		n, _ := strconv.Atoi(at(args, 1))
		var ids []string
		for range max(n, 1) {
			id, _, err := c.placeAt(seller, customer, priceSuccess)
			if err != nil {
				fmt.Fprintln(os.Stderr, "sipariş:", err)
				return 1
			}
			ids = append(ids, id)
		}
		fmt.Println(strings.Join(ids, " "))
		return 0
	case "status":
		for _, id := range args[1:] {
			o, err := c.order(customer.AccessToken, id)
			if err != nil {
				fmt.Fprintln(os.Stderr, id, err)
				return 1
			}
			fmt.Fprintf(os.Stderr, "  %s  %s\n", id[:8], o)
		}
		return c.printOutbox()
	case "await":
		secs, _ := strconv.Atoi(at(args, 1))
		ids := args[2:]
		deadline := time.Now().Add(time.Duration(max(secs, 1)) * time.Second)
		start := time.Now()
		for {
			paid := 0
			for _, id := range ids {
				if o, err := c.order(customer.AccessToken, id); err == nil && o.Status == "paid" {
					paid++
				}
			}
			if paid == len(ids) {
				fmt.Fprintf(os.Stderr, "  %d/%d sipariş ödendi (%v içinde)\n", paid, len(ids), time.Since(start).Round(time.Second))
				return c.printOutbox()
			}
			if time.Now().After(deadline) {
				fmt.Fprintf(os.Stderr, "  ZAMAN AŞIMI: %d/%d sipariş ödendi\n", paid, len(ids))
				_ = c.printOutbox()
				return 1
			}
			time.Sleep(time.Second)
		}
	}
	fmt.Fprintln(os.Stderr, "bilinmeyen adım:", args[0])
	return 2
}

func (c *client) printOutbox() int {
	ops, err := c.loginAs(auth.RoleOperations)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	r, err := c.get("/api/v1/ops/outbox", ops.AccessToken)
	if err != nil || r.status != http.StatusOK {
		fmt.Fprintln(os.Stderr, "outbox okunamadı:", err)
		return 1
	}
	var s struct {
		Pending, Retrying, Dead int
		Problems                []struct {
			EventType string  `json:"event_type"`
			Attempts  int     `json:"attempts"`
			LastError *string `json:"last_error"`
		}
	}
	_ = r.decode(&s)
	fmt.Fprintf(os.Stderr, "  outbox: bekleyen=%d yeniden-deneniyor=%d bırakılmış=%d\n", s.Pending, s.Retrying, s.Dead)
	for i, p := range s.Problems {
		if i == 3 {
			break
		}
		msg := ""
		if p.LastError != nil {
			msg = *p.LastError
			if len(msg) > 90 {
				msg = msg[:90] + "…"
			}
		}
		fmt.Fprintf(os.Stderr, "    %s deneme=%d hata=%q\n", p.EventType, p.Attempts, msg)
	}
	return 0
}

func at(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}
