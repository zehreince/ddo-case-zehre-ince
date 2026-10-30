# ÇarşıHub — Çok Satıcılı Pazaryeri Sipariş ve Ödeme Sistemi

**Geliştiren:** Zehre İnce · DDO Yazılım Ekibi alım projesi

Satıcıların ürün sattığı bir pazaryerinin sepet, stok, çok satıcılı sipariş bölme, ödeme, komisyon ve iade süreçlerini yöneten sistem.

> Durum: kimlik doğrulama ve RBAC tamam (6 rol, JWT + refresh rotasyonu). Sırada katalog ve stok (F-1, T-1).

## Kurulum

Gereksinim: Docker + Docker Compose.

```bash
git clone https://github.com/zehreince/ddo-case-zehre-ince.git && cd ddo-case-zehre-ince
cp .env.example .env
docker compose up --build
```

| Servis | Adres |
|---|---|
| Frontend | http://localhost:3000 |
| API | http://localhost:8080 |
| Sağlık | http://localhost:8080/health |
| Metrikler | http://localhost:8080/metrics |

Veritabanı host'a açılmaz, yalnızca compose ağı içinden erişilir.

### Demo hesapları

Seed her açılışta otomatik yüklenir. Hepsinin parolası **`Demo123!`**; giriş ekranındaki "Demo hesapları" düğmeleri formu doldurur.

| Rol | E-posta |
|---|---|
| Müşteri | `musteri@carsihub.test` |
| Satıcı | `satici@carsihub.test` |
| Pazaryeri operasyon | `operasyon@carsihub.test` |
| Finans | `finans@carsihub.test` |
| Kargo entegrasyonu | `kargo@carsihub.test` |
| Yönetici | `yonetici@carsihub.test` |

### Sık kullanılan komutlar

| Komut | Ne yapar |
|---|---|
| `make setup` | Git hook'larını açar: commit'te gofmt/vet, push'ta tüm testler |
| `make up` | Derler, ayağa kaldırır, sağlıklı olana kadar bekler |
| `make verify` | Tüm kontrolleri uçtan uca çalıştırır, PASS/FAIL tablosu basar |
| `make reset-demo` | Veritabanını silip ilk seed haline döndürür |
| `make test` | Backend + frontend birim testleri |
| `make lint` | go vet, golangci-lint, TypeScript kontrolü |

## Mimari

```
frontend (React, nginx :3000) ──► api (Go :8080) ──► PostgreSQL 16
                                      ▲
                     migrate (bir kez çalışır, compose'da api'den önce)
```

- **Backend:** Go, tek modül. Alan bazlı paketler (`internal/catalog`, `internal/order` …); her paketin içinde `domain → service → store → handler` katmanları. Ayrıntı: [DECISIONS.md](DECISIONS.md).
- **Ortak altyapı:** `internal/platform` — config, JSON log, hata biçimi, middleware, veritabanı, metrikler.
- **Komutlar:** `cmd/api`, `cmd/migrate`, `cmd/verify` (ileride `cmd/worker`, `cmd/simulator`). Hepsi tek imajda; compose hangisinin çalışacağını seçer.
- **Frontend:** React + Vite + TypeScript.

### Frontend seçimi ve gerekçesi

React + Vite + TypeScript. Rol bazlı birden fazla panel (müşteri, satıcı, finans, operasyon) olacağı için bileşen yapısı ve tip güvenliği işe yarıyor. Vite derlemesi hızlı, çıktı nginx ile statik olarak sunulabiliyor. HTMX daha sade olurdu ama sepet ve kupon hesabı gibi istemci tarafı durumu olan ekranlarda React daha rahat.

## Doğrulama Soruları

_Proje ilerledikçe doldurulacak._
