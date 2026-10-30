// Package migrations SQL migrasyon dosyalarını ikili dosyaya gömer;
// böylece migrate imajı ayrıca dosya kopyalamaya ihtiyaç duymaz.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
