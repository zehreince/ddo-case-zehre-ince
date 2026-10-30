// Package pagination keyset (imleç) sayfalaması için ortak yardımcılardır.
// OFFSET yerine "son gördüğüm kayıttan sonrası" sorgulanır; sayfa derinliği
// arttıkça sorgu yavaşlamaz ve araya eklenen kayıtlar sayfaları kaydırmaz.
package pagination

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

var ErrBadCursor = errors.New("geçersiz imleç")

// Cursor (created_at, id) çiftidir; id eşit zaman damgalarında sırayı kesinleştirir.
type Cursor struct {
	CreatedAt time.Time `json:"t"`
	ID        string    `json:"i"`
}

func (c Cursor) Encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Decode boş dizgi için (nil, nil) döner: ilk sayfa.
func Decode(s string) (*Cursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, ErrBadCursor
	}
	var c Cursor
	if err := json.Unmarshal(b, &c); err != nil || c.ID == "" || c.CreatedAt.IsZero() {
		return nil, ErrBadCursor
	}
	return &c, nil
}

// Limit sorgu parametresini [1, max] aralığında okur.
func Limit(s string, def, max int) (int, error) {
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > max {
		return 0, errors.New("limit 1 ile " + strconv.Itoa(max) + " arasında olmalı")
	}
	return n, nil
}

// Page bir sayfanın JSON biçimidir. NextCursor boşsa son sayfadır.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}
