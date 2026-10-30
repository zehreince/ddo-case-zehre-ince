import { useState, type FormEvent } from 'react'
import { ApiError } from '../api/client'
import { useAuth } from '../auth/AuthContext'
import { roleLabels, type Role } from '../auth/roles'

// Değerlendirme kolaylığı için demo hesapları tek tıkla doldurulur (bkz. TESTING.md).
const demoAccounts: { email: string; role: Role }[] = [
  { email: 'musteri@carsihub.test', role: 'customer' },
  { email: 'satici@carsihub.test', role: 'seller' },
  { email: 'operasyon@carsihub.test', role: 'operations' },
  { email: 'finans@carsihub.test', role: 'finance' },
  { email: 'kargo@carsihub.test', role: 'carrier' },
  { email: 'yonetici@carsihub.test', role: 'admin' },
]
const DEMO_PASSWORD = 'Demo123!'

export function LoginPage() {
  const { login } = useAuth()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [fields, setFields] = useState<Record<string, string>>({})

  async function onSubmit(e: FormEvent) {
    e.preventDefault()
    setPending(true)
    setError(null)
    setFields({})
    try {
      await login(email, password)
    } catch (err) {
      if (err instanceof ApiError) {
        setFields(err.fields)
        setError(err.code === 'validation_failed' ? null : err.message)
      } else {
        setError('Beklenmeyen bir hata oluştu')
      }
    } finally {
      setPending(false)
    }
  }

  return (
    <main className="login">
      <section className="card login-card">
        <h1>ÇarşıHub</h1>
        <p className="muted">Hesabınızla giriş yapın</p>

        <form onSubmit={onSubmit} noValidate>
          <label>
            E-posta
            <input
              type="email"
              autoComplete="username"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              aria-invalid={!!fields.email}
            />
            {fields.email && <span className="field-error">{fields.email}</span>}
          </label>
          <label>
            Parola
            <input
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              aria-invalid={!!fields.password}
            />
            {fields.password && <span className="field-error">{fields.password}</span>}
          </label>
          {error && (
            <p className="error" role="alert">
              {error}
            </p>
          )}
          <button type="submit" disabled={pending}>
            {pending ? 'Giriş yapılıyor…' : 'Giriş yap'}
          </button>
        </form>

        <details className="demo">
          <summary>Demo hesapları</summary>
          <p className="muted">Hepsinin parolası {DEMO_PASSWORD}</p>
          <div className="demo-list">
            {demoAccounts.map((a) => (
              <button
                key={a.email}
                type="button"
                className="secondary"
                onClick={() => {
                  setEmail(a.email)
                  setPassword(DEMO_PASSWORD)
                }}
              >
                {roleLabels[a.role]}
              </button>
            ))}
          </div>
        </details>
      </section>
    </main>
  )
}
