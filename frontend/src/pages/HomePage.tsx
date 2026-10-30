import { useAuth } from '../auth/AuthContext'
import { roleLabels } from '../auth/roles'
import { SystemStatus } from '../components/SystemStatus'

export function HomePage() {
  const { state } = useAuth()
  if (state.status !== 'authenticated') return null
  const { user } = state

  return (
    <>
      <section className="card">
        <h2>Hesabım</h2>
        <dl className="facts">
          <dt>Ad</dt>
          <dd>{user.full_name}</dd>
          <dt>E-posta</dt>
          <dd>{user.email}</dd>
          <dt>Rol</dt>
          <dd>{roleLabels[user.role]}</dd>
        </dl>
      </section>
      <SystemStatus />
    </>
  )
}
