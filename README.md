# ÇarşıHub — Çok Satıcılı Pazaryeri Sipariş ve Ödeme Sistemi

**Geliştiren:** Zehre İnce · DDO Yazılım Ekibi alım projesi

Satıcıların ürün sattığı bir pazaryerinin sepet, stok, çok satıcılı sipariş bölme, ödeme, komisyon ve iade süreçlerini yöneten sistem.

> Durum: kimlik doğrulama/RBAC, katalog ve satıcı bazlı stok (F-1), sepet (F-2), çok satıcılı sipariş (F-3) ve **T-1 stok rezervasyonu / oversell önleme** tamam. Sırada ödeme ve saga/outbox (F-4, T-2). Arayüzde şimdilik giriş ve yönetici ekranları var; katalog ve sepet ekranları sonraki adımda.

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
frontend (React, nginx :3000) ──► api (Go :8080) ──► PostgreSQL 16 ◄── worker (Go :8081)
                                                          ▲            süresi dolan rezervasyon
                              migrate (bir kez çalışır:   │            ve siparişleri süpürür
                              şema + demo seed) ──────────┘
```

- **Backend:** Go, tek modül. Alan bazlı paketler (`internal/catalog`, `internal/order` …); her paketin içinde `domain → service → store → handler` katmanları. Ayrıntı: [DECISIONS.md](DECISIONS.md).
- **Ortak altyapı:** `internal/platform` — config, JSON log, hata biçimi, middleware, veritabanı, metrikler.
- **Komutlar:** `cmd/api`, `cmd/migrate`, `cmd/worker`, `cmd/verify` (ileride `cmd/simulator`). Hepsi tek imajda; compose hangisinin çalışacağını seçer.
- **Stok (T-1):** Oversell tek bir koşullu `UPDATE` ile engellenir, sistem genelinde sabit kilit sırası deadlock'u önler. Ayrıntı ve elenen alternatifler: [ADR-007](DECISIONS.md#adr-007-stok-rezervasyonu--koşullu-update-ayrı-rezervasyon-tablosu-global-kilit-sırası).
- **Frontend:** React + Vite + TypeScript.

### Frontend seçimi ve gerekçesi

React + Vite + TypeScript. Rol bazlı birden fazla panel (müşteri, satıcı, finans, operasyon) olacağı için bileşen yapısı ve tip güvenliği işe yarıyor. Vite derlemesi hızlı, çıktı nginx ile statik olarak sunulabiliyor. HTMX daha sade olurdu ama sepet ve kupon hesabı gibi istemci tarafı durumu olan ekranlarda React daha rahat.

## Doğrulama Soruları

_1–4 proje tamamlandıkça ölçümlerle doldurulacak._

### 5. "Stok rezervasyonu ve oversell önleme" çözümünün doğru çalıştığını nasıl kanıtladın?

Üç katmanda, hepsi gerçek PostgreSQL 16 üzerinde (mock yok):

**a) Stok seviyesinde** — aynı teklife (stok 50) 1.000 goroutine aynı anda rezervasyon ister:

```bash
cd backend && go test ./internal/inventory -run ThousandConcurrent -v
```
```
inventory_test.go:67: istek=1000 başarılı=50 stok-yetersiz=950 | on_hand=50 reserved=50
--- PASS: TestReserve_ThousandConcurrentRequestsNeverOversell (5.72s)
```
Test ayrıca `offers.stock_reserved` ile aktif rezervasyonların toplamının eşit olduğunu doğrular.

**b) Sipariş seviyesinde** — stok 100, 1.000 eşzamanlı sipariş:

```bash
cd backend && go test ./internal/order -run ThousandConcurrentOrders -v
```
```
service_test.go:147: sipariş isteği=1000 oluşan=100 reddedilen=900 | sipariş tablosu=100 kalem toplamı=100 reserved=100
--- PASS: TestPlace_ThousandConcurrentOrdersNeverOversell (8.36s)
```

**c) Çalışan sistemde HTTP üzerinden** — `make verify` içindeki T-1a, stok 50 olan teklife 1.000 paralel `POST /api/v1/orders` atar, ardından siparişleri iptal edip stoğun tamamen döndüğünü kontrol eder:

```
  T-1a: 201=50 409=950 diğer=0 reserved=50/50 süre=2.57s
T-1a     1.000 eşzamanlı sipariş: stoktan fazlası satılmaz                      PASS   3.556s
```

Bunlara ek olarak: ters sırada kilit isteyen 100 eşzamanlı işlemin deadlock'a girmediği, doğrudan SQL ile eldekinden fazla rezervasyon yazılamadığı (`CHECK` kısıtı), TTL dolunca ve ödeme başarısız olunca stoğun iade edildiği ayrı testlerle gösterildi. Komutların tamamı: [TESTING.md → Kanıt komutları](TESTING.md#kanıt-komutları).
