package logx

import (
	"bytes"
	"strings"
	"testing"
)

func TestNew_RedactsSensitiveKeys(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, "info").Info("giris", "email", "a@b.c", "password", "gizli123", "refresh_token", "tkn")

	out := buf.String()
	if strings.Contains(out, "gizli123") || strings.Contains(out, "tkn") {
		t.Fatalf("hassas değer loga sızdı: %s", out)
	}
	if !strings.Contains(out, "a@b.c") {
		t.Fatalf("hassas olmayan değer kayboldu: %s", out)
	}
}

func TestNew_RespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, "warn")
	l.Info("gorunmemeli")
	l.Warn("gorunmeli")
	if strings.Contains(buf.String(), "gorunmemeli") || !strings.Contains(buf.String(), "gorunmeli") {
		t.Fatalf("seviye filtresi çalışmıyor: %s", buf.String())
	}
}
