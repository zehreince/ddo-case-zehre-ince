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
| F-4 / F-6 | Fiyatı ,05 ile biten ürünü sipariş et, `GET /orders/{id}` ile izle | Ödeme `succeeded` → stok eldekinden düşer → satıcıya bildirim → kargo `picked_up → in_transit → delivered` (~11 sn). `GET /shipments/{tracking}` 3 olay gösterir (tekrar gelen teslim olayı tekilleşir) | ✅ |
| T-2a | Fiyatı ,01 ile biten ürün | Ödeme reddedilir → **telafi**: sipariş `cancelled`, stok iade | ✅ |
| T-2b | Fiyatı ,03 ile biten ürün | Callback iki kez gelir (`callback_count=2`), stok bir kez düşer | ✅ |
| T-2c | Fiyatı ,02 ile biten ürün | Callback hiç gelmez (zaman aşımı); worker'ın mutabakatı sağlayıcıya sorar ve siparişi tamamlar (~12 sn) | ✅ |
| T-2d | ,02 ürün; ödeme sağlayıcıya ulaştıktan sonra müşteri iptal eder | Sipariş iptal, stok iade; sonra gelen başarılı ödeme → **telafi**: otomatik iade (`refunded`) | ✅ |
| T-2e | Operasyon hesabıyla `GET /api/v1/ops/outbox` | Bırakılmış (dead) olay yok; müşteri 403 alır | ✅ |
| WEBHOOK | İmzasız `POST /payments/callback` ve `/shipping/callback` | 401 `invalid_signature` | ✅ |
| T-3a | Üç satıcı satırı + başka satıcı; pazaryeri (sabit) + satıcı (yüzde) kuponu ile sipariş | İki kupon da uygulanır; satıcı kuponu yalnızca kendi satırlarına; alt sipariş ve satır tutarları sipariş toplamına kuruşu kuruşuna eşit | ✅ |
| T-3b | Min. sepeti tutmayan, kategorisi uymayan ve bilinmeyen kupon; sonra birleştirilemeyen + birleştirilebilen kupon | Nedenleriyle reddedilir (`min_basket`, `no_eligible_items`, `not_found`); daha avantajlı birleştirilemeyen kupon seçilir, diğeri `not_combinable` | ✅ |
| T-3c | 3 kullanım hakkı olan kuponla 20 eşzamanlı sipariş | Tam 3'ü indirimli; diğerleri indirimsiz oluşur (`usage_limit`) | ✅ |
| T-3d | Min. 250 TL kuponla 300 TL sipariş, ödeme sonrası 100 TL'lik kalem için `POST /orders/{id}/refund-quote` | Kalan sepet min. altına düşer → kupon geri alınır: iade 50,05 TL, kesinti (clawback) gösterilir | ✅ |
| F-5, F-7 | | | ⏳ |
| T-4, T-5 | | | ⏳ |

### Demo kuponlar

| Kod | Tür | Kural |
|---|---|---|
| `PAZAR50` | Pazaryeri, 50 TL | Min. sepet 300 TL, birleştirilebilir |
| `YUZDE10` | Pazaryeri, %10 (en çok 150 TL) | Birleştirilemez: diğer kuponlarla verilirse daha avantajlı seçenek kullanılır |
| `DEMOGIYIM20` | Demo Mağaza, %20 | Yalnızca Giyim kategorisindeki Demo Mağaza ürünleri |
| `ILK100` | Pazaryeri, 100 TL | Min. sepet 500 TL, toplam 100 kullanım, kişi başı 1 |

### Ödeme simülatörü: test kartları gibi kuruş değerleri

Sipariş **toplamının** kuruş hanesi simülatörün davranışını belirler (tek adet sipariş verin; 2 adet toplamı değiştirir):

| Kuruş | Davranış |
|---|---|
| `,01` | Ödeme reddedilir |
| `,02` | Ödeme başarılı ama callback kaybolur (yalnızca sorgulayınca görülür) |
| `,03` | Ödeme başarılı, callback iki kez gönderilir |
| `,04` | Ödeme hiç sonuçlanmaz (sağlayıcıda askıda) |
| `,05` | Her zaman başarılı |
| diğer | `SIM_SEED` ile deterministik; `SIM_PAYMENT_*_RATE` oranlarında ret / kayıp / çift callback |

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
| **T-2** kaos | `make chaos` | Ödeme sağlayıcı durdurulur → 5 sipariş alınır, olaylar outbox'ta birikir ve yeniden denenir → sağlayıcı dönünce hepsi ödenir. Ardından worker `SIGKILL` ile öldürülür → yeniden başlayınca kaldığı yerden devam eder. Bırakılmış olay 0 |
| **T-2** outbox | `cd backend && go test ./internal/outbox -v` | Olay işlemle atomik; 4 worker eşzamanlı → her olay tam bir kez; çöken worker'ın kiraladığı olay devralınır; küme içi sıra korunur; yavaş bağımlılık kuyruğu tıkamaz; deneme sınırında `dead` |
| **T-2** idempotent tüketici | `cd backend && go test ./internal/outbox -run Once -v` | Aynı olay 10 kez eşzamanlı gelse de etki bir kez |
| **T-2** ödeme | `cd backend && go test ./internal/payment -v` | Çift/eşzamanlı callback → tek etki; ret → stok iadesi; kapalı siparişe geç ödeme → bir kez iade; mutabakat: kayıp callback, sağlayıcının unuttuğu ödeme, askıda ödeme |
| **T-2** kargo | `cd backend && go test ./internal/shipping -v` | Gönderi bir kez oluşturulur; tekrar eden / sırasız kargo olayı durumu geri almaz |
| Simülatör | `cd backend && go test ./internal/simulator -v` | Aynı tohum → aynı sonuç; HTTP istemcilerimiz gerçek simülatöre karşı her senaryoda |
| **T-3** fiyat motoru | `cd backend && go test ./internal/pricing -v -cover` | Yüzde/sabit/üst sınır, min. sepet, kategori, kapsam çakışması, birlikte kullanım seçimi; rastgele 2.000 sepette kuruş kayması yok, sonuç satır sırasından bağımsız; kademeli tam iadede iadeler toplamı ödenen tutara eşit; kapsama %99+ |
| **T-3** dağıtım | `cd backend && go test ./internal/pricing -run Allocate -v` | En büyük kalan yöntemi; 128-bit ara çarpımla taşma yok |
| **T-3** kullanım limiti | `cd backend && go test ./internal/order -run CouponUsageLimit -v` | 10 hakkı olan kupona 60 eşzamanlı sipariş → tam 10 indirimli (`redeemed_count` = aktif kullanım = 10) |
| **T-3** kısmi iade | `cd backend && go test ./internal/pricing -run Refund -v` | Yüzde kuponda iade edilen kalemin indirimi de geri alınır; min. sepet altına düşünce kupon geri alınır (clawback); iade asla negatif değil |
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
8. Saga'yı izlemek: `requests.http` → "Ödeme ve kargo" bölümü. Fiyatı `,05` ile biten bir ürün ekleyip sipariş verin; `GET /orders/{id}` yanıtında `payment.status` ve `sub_orders[].shipment.status` birkaç saniye içinde ilerler. `make chaos` bağımlılık kesintisini canlı gösterir.
9. Tarayıcıyı telefon genişliğine (≈375 px) daralt: menü ve kartlar alt alta dizilir, yatay kaydırma olmaz.
10. `docker compose stop db` ile veritabanını durdur: "Sistem durumu" kartı birkaç saniye içinde hata durumuna geçer. `docker compose start db` ile geri döner.
