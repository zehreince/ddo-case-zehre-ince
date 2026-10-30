import { useEffect, useState } from 'react'
import { ApiError, publicRequest } from '../api/client'
import { ErrorBox, Loading } from './Status'

type Health = { status: string; database: string }

type State = { kind: 'loading' } | { kind: 'ready'; health: Health } | { kind: 'error'; message: string }

const POLL_MS = 5000

// Sistemin canlı olduğunu gösteren kart; 5 sn'de bir /health'i yoklar.
export function SystemStatus() {
  const [state, setState] = useState<State>({ kind: 'loading' })

  useEffect(() => {
    const ctrl = new AbortController()
    const load = () =>
      publicRequest<Health>('/health', { signal: ctrl.signal })
        .then((health) => setState({ kind: 'ready', health }))
        .catch((e) => {
          if (ctrl.signal.aborted) return
          setState({ kind: 'error', message: e instanceof ApiError ? e.message : 'Beklenmeyen hata' })
        })
    void load()
    const id = setInterval(load, POLL_MS)
    return () => {
      ctrl.abort()
      clearInterval(id)
    }
  }, [])

  return (
    <section className="card" aria-live="polite">
      <h2>Sistem durumu</h2>
      {state.kind === 'loading' && <Loading />}
      {state.kind === 'error' && <ErrorBox message={state.message} />}
      {state.kind === 'ready' && (
        <p className={`status ${state.health.status === 'ok' ? 'good' : 'bad'}`}>
          API: {state.health.status} · Veritabanı: {state.health.database}
        </p>
      )}
    </section>
  )
}
