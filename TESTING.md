# Test ve Doğrulama Rehberi

## Hızlı doğrulama

```bash
cp .env.example .env
make verify
```

Sistemi ayağa kaldırır, her kontrolü çalıştırır ve sonunda bir PASS/FAIL tablosu basar. Bir kontrol bile düşerse çıkış kodu sıfırdan farklıdır. Tekrarlamak için önce `make reset-demo`.

## Demo hesapları

_Kimlik doğrulama adımında eklenecek._

| Rol | E-posta | Parola |
|---|---|---|

## Doğrulama kimlikleri

| Kimlik | Nasıl denenir | Beklenen sonuç | Durum |
|---|---|---|---|
| HEALTH | `GET /health` | 200, `"database":"ok"` | ✅ |
| API-ERR | `GET /api/v1/olmayan-uc` | 404, `{"error":{"code":"not_found",...}}` | ✅ |
| METRICS | `GET /metrics` | Prometheus metinleri | ✅ |
| F-1 … F-7 | | | ⏳ |
| T-1 … T-5 | | | ⏳ |

## Kanıt komutları

| Görev | Komut | Ne gösterir |
|---|---|---|
| Birim testleri | `make test` | Backend + frontend testleri, backend kapsama oranı |

## Arayüzde elle deneme

1. http://localhost:3000 adresini aç; "Sistem durumu" kartında API ve veritabanı `ok` görünmeli.
2. `docker compose stop db` ile veritabanını durdur; kart birkaç saniye içinde hata durumuna geçmeli. `docker compose start db` ile geri döner.
