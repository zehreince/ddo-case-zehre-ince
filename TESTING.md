# Test ve Doğrulama Rehberi

## Hızlı doğrulama

```bash
cp .env.example .env
make verify
```

Sistemi ayağa kaldırır, her kontrolü çalıştırır ve sonunda bir PASS/FAIL tablosu basar. Bir kontrol bile düşerse çıkış kodu sıfırdan farklıdır. Tekrarlamak için önce `make reset-demo`.

## Demo hesapları

Migrasyon servisi her açılışta bu hesapları oluşturur (varsa dokunmaz). **Hepsinin parolası `Demo123!`.** Giriş ekranındaki "Demo hesapları" düğmeleri formu doldurur.

| Rol | E-posta | Parola |
|---|---|---|
| Müşteri | `musteri@carsihub.test` | `Demo123!` |
| Satıcı | `satici@carsihub.test` | `Demo123!` |
| Pazaryeri operasyon | `operasyon@carsihub.test` | `Demo123!` |
| Finans | `finans@carsihub.test` | `Demo123!` |
| Kargo entegrasyonu | `kargo@carsihub.test` | `Demo123!` |
| Yönetici | `yonetici@carsihub.test` | `Demo123!` |

## Doğrulama kimlikleri

| Kimlik | Nasıl denenir | Beklenen sonuç | Durum |
|---|---|---|---|
| HEALTH | `GET /health` | 200, `"database":"ok"` | ✅ |
| API-ERR | `GET /api/v1/olmayan-uc` | 404, `{"error":{"code":"not_found",...}}` | ✅ |
| METRICS | `GET /metrics` | Prometheus metinleri | ✅ |
| AUTH-1 | Her demo hesapla `POST /api/v1/auth/login` | 200, access + refresh token, doğru rol | ✅ |
| AUTH-2 | Yanlış parolayla giriş | 401 `invalid_credentials` (bilinmeyen e-postayla aynı yanıt) | ✅ |
| AUTH-3 | `{"email":"bozuk","password":""}` ile giriş | 422 `validation_failed`, `fields.email` ve `fields.password` | ✅ |
| AUTH-4 | Token'sız ya da sahte token'la `GET /api/v1/auth/me` | 401 `unauthorized` | ✅ |
| AUTH-5 | Refresh token'ı iki kez kullan | İlki 200 ve yeni token; ikincisi 401 `token_reused`, yeni token da iptal | ✅ |
| AUTH-6 | `POST /api/v1/auth/logout` sonrası refresh | 204, ardından 401 | ✅ |
| RBAC-1 | Yönetici dışı 5 rolle `GET /api/v1/admin/users` | 403 `forbidden` | ✅ |
| RBAC-2 | Yöneticiyle `GET /api/v1/admin/users?limit=2`, `next_cursor` ile devam | Tekrarsız, eksiksiz liste | ✅ |
| F-1 | Satıcı `POST /api/v1/seller/products` ile varyantlı ürün ekler; müşteri `GET /api/v1/products/{id}` | 201; aynı varyantı birden fazla satıcı kendi fiyat/stoğuyla satabilir | ✅ |
| F-1b | Sipariş stok tutarken satıcı `PUT /seller/offers/{id}/stock` ile stoğu tutulanın altına indirmeye çalışır | 409 `below_reserved`; üstüne çıkarmak 200 | ✅ |
| F-2 | `POST /api/v1/cart/items`, adet değiştir, `DELETE /cart/items/{id}` | Sepet stok tutar (`reserved_until`), adet değişince rezerve miktar değişir, çıkarınca stok döner | ✅ |
| F-3 | İki farklı satıcının ürünüyle sepet → `POST /api/v1/cart/checkout` | 201, tek sipariş, satıcı başına bir alt sipariş, toplamlar tutarlı | ✅ |
| T-1a | Stok 50 olan teklife 1.000 eşzamanlı `POST /api/v1/orders` | Tam 50 × 201, 950 × 409 `insufficient_stock`; rezerve 50/50. İptal edilince stok tamamen döner | ✅ |
| T-1b | Biri stoklu, biri yetersiz iki kalemli sipariş | `allow`: 201, yalnızca stoklu kalem; `unavailable_items` listesi. `reject`: 409, hiçbir stok tutulmaz | ✅ |
| T-1c | Aynı `Idempotency-Key` ile 3 kez sipariş, sonra farklı gövdeyle | Aynı sipariş kimliği, stok bir kez tutulur; farklı gövde 422 `idempotency_key_reused` | ✅ |
| F-4 … F-7 | | | ⏳ |
| T-2 … T-5 | | | ⏳ |

## Kanıt komutları

| Görev | Komut | Ne gösterir |
|---|---|---|
| Birim + entegrasyon | `make test` | Backend testleri (entegrasyon testleri testcontainers ile gerçek PostgreSQL 16 açar, Docker gerekir), `internal/` kapsaması ve %70 eşiği; frontend testleri |
| **T-1** oversell (stok seviyesi) | `cd backend && go test ./internal/inventory -run ThousandConcurrent -v` | 1.000 eşzamanlı rezervasyon → tam 50 başarılı (stok 50), `stock_reserved` = aktif rezervasyon toplamı |
| **T-1** oversell (sipariş seviyesi) | `cd backend && go test ./internal/order -run ThousandConcurrentOrders -v` | 1.000 eşzamanlı sipariş → tam 100 sipariş (stok 100); sipariş, kalem ve rezerve sayıları tutarlı |
| **T-1** HTTP üzerinden | `make verify` (T-1a satırı) | Çalışan sistemde 1.000 paralel istek; özet satırı stderr'e basılır |
| **T-1** kısmi satış | `cd backend && go test ./internal/order ./internal/cart -run "Policy" -v` | `allow` ve `reject` kuralları hem doğrudan siparişte hem sepetten ödemede |
| **T-1** TTL ve iade | `cd backend && go test ./internal/inventory ./internal/sweeper ./internal/order -run "Expire|Sweep|PaymentOutcomes" -v` | Süresi dolan sepet rezervasyonu ve ödenmeyen sipariş stoğu iade eder; ödeme başarısız → iade, başarılı → stoktan düşer |
| **T-1** deadlock | `cd backend && go test ./internal/inventory -run OppositeOrder -v` | Aynı iki ürünü ters sırada isteyen 100 eşzamanlı işlem deadlock'a girmez |
| **T-1** son savunma | `cd backend && go test ./internal/inventory -run CheckConstraint -v` | Uygulama atlanıp doğrudan SQL ile eldekinden fazla rezervasyon yazılamaz |
| Idempotency | `cd backend && go test ./internal/platform/idempotency -v` | Aynı anahtar → işlem bir kez; eşzamanlı aynı anahtar → 1 × 201 + 9 × 409 |
| Refresh yarışı | `cd backend && go test ./internal/auth -run ConcurrentRequests -v -count=5` | Aynı refresh token'la 10 eşzamanlı istekten her seferinde tam 1'i başarılı (`SELECT … FOR UPDATE`) |
| Token çalınması | `cd backend && go test ./internal/auth -run ReuseRevokesWholeFamily -v` | İptal edilmiş token kullanılınca oturum ailesinin tamamı iptal edilir, audit'e yazılır |
| Denetim kaydı | `cd backend && go test ./internal/platform/migrate -run AuditLog -v` | `audit_log` üzerinde UPDATE/DELETE/TRUNCATE veritabanınca reddedilir (42501) |
| Migrasyonlar | `cd backend && go test ./internal/platform/migrate -v` | Migrasyonlar tekrar çalıştırılabilir ve geri alınabilir |
| Lint | `make lint` | CI ile aynı golangci-lint sürümü |

## Arayüzde elle deneme

1. http://localhost:3000 adresini aç. Giriş ekranı gelir.
2. Yanlış parolayla dene: "e-posta ya da parola hatalı" mesajı görünür.
3. "Demo hesapları" → **Yönetici** → Giriş yap. Üst menüde "Kullanıcılar" görünür; tıklayınca 6 demo kullanıcı listelenir.
4. Sayfayı yenile (F5): oturum korunur (refresh token httpOnly çerezde, access token yalnızca bellekte).
5. **Çıkış** → sayfayı yenile: giriş ekranına dönülür.
6. **Müşteri** ile gir: menüde "Kullanıcılar" yoktur. Adres çubuğuna `http://localhost:3000/admin/users` yaz: "Erişim yok" ekranı (API de 403 döner).
7. API ile stok senaryosu (`requests.http` → "Katalog ve stok" bölümü): satıcı olarak ürün ekle, müşteri olarak sepete ekle; `GET /api/v1/cart` yanıtında `reserved_until` görünür. Seed'deki **Çelik Termos** (stok 3) ve **Şişme Mont L** (stok 1) kısmi satışı denemek için bilerek düşük stokludur.
8. Tarayıcıyı telefon genişliğine (≈375 px) daralt: menü ve kartlar alt alta dizilir, yatay kaydırma olmaz.
9. `docker compose stop db` ile veritabanını durdur: "Sistem durumu" kartı birkaç saniye içinde hata durumuna geçer. `docker compose start db` ile geri döner.
