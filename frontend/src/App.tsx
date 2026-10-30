import { useEffect, useState } from 'react'
import { ApiError, apiGet } from './api/client'

type Health = { status: string; database: string }

type State =
  | { kind: 'loading' }
  | { kind: 'ready'; health: Health }
  | { kind: 'error'; message: string }

const POLL_MS = 5000

export function App() {
  const [state, setState] = useState<State>({ kind: 'loading' })

  useEffect(() => {
    const ctrl = new AbortController()
    const load = () =>
      apiGet<Health>('/health', ctrl.signal)
        .then((health) => setState({ kind: 'ready', health }))
        .catch((e) => {
          if (ctrl.signal.aborted) return
          setState({ kind: 'error', message: e instanceof ApiError ? e.message : 'Beklenmeyen hata' })
        })
    load()
    const id = setInterval(load, POLL_MS)
    return () => {
      ctrl.abort()
      clearInterval(id)
    }
  }, [])

  return (
    <main className="shell">
      <header>
        <h1>ÇarşıHub</h1>
        <p className="muted">Çok satıcılı pazaryeri</p>
      </header>

      <section className="card" aria-live="polite">
        <h2>Sistem durumu</h2>
        {state.kind === 'loading' && <p className="muted">Yükleniyor…</p>}
        {state.kind === 'error' && <p className="status bad">{state.message}</p>}
        {state.kind === 'ready' && (
          <p className={`status ${state.health.status === 'ok' ? 'good' : 'bad'}`}>
            API: {state.health.status} · Veritabanı: {state.health.database}
          </p>
        )}
      </section>
    </main>
  )
}
