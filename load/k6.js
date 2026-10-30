// k6 yük testi: oturum açmış müşterinin okuma yükü (katalog, ürün, sepet, sağlık).
// Çalıştırma: make load   (sistem ayakta olmalı: make up)
import http from 'k6/http';
import { check, sleep } from 'k6';

const BASE = __ENV.BASE_URL || 'http://localhost:8080';

export const options = {
  scenarios: {
    okuma: { executor: 'constant-vus', vus: Number(__ENV.VUS || 20), duration: __ENV.DURATION || '30s' },
  },
  summaryTrendStats: ['med', 'p(95)', 'p(99)', 'max'],
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<300'],
  },
};

export function setup() {
  const r = http.post(`${BASE}/api/v1/auth/login`, JSON.stringify({ email: 'musteri@carsihub.test', password: 'Demo123!' }), {
    headers: { 'Content-Type': 'application/json' },
  });
  const token = r.json('access_token');
  const list = http.get(`${BASE}/api/v1/products?limit=20`, { headers: { Authorization: `Bearer ${token}` } });
  return { token, ids: list.json('items').map((p) => p.id) };
}

export default function (data) {
  const h = { headers: { Authorization: `Bearer ${data.token}` }, tags: {} };
  h.tags.name = 'GET /products';
  check(http.get(`${BASE}/api/v1/products?limit=20`, h), { 'liste 200': (r) => r.status === 200 });
  h.tags.name = 'GET /products/{id}';
  const id = data.ids[Math.floor(Math.random() * data.ids.length)];
  check(http.get(`${BASE}/api/v1/products/${id}`, h), { 'ürün 200': (r) => r.status === 200 });
  h.tags.name = 'GET /cart';
  check(http.get(`${BASE}/api/v1/cart`, h), { 'sepet 200': (r) => r.status === 200 });
  check(http.get(`${BASE}/health`, { tags: { name: 'GET /health' } }), { 'health 200': (r) => r.status === 200 });
  sleep(0.1);
}
