import { useCallback, useEffect, useState } from 'react'
import { ApiError } from '../api/client'
import { useAuth } from '../auth/AuthContext'
import { roleLabels, type User } from '../auth/roles'
import { Empty, ErrorBox, Loading } from '../components/Status'

type Page = { items: User[]; next_cursor?: string }

const PAGE_SIZE = 20

export function UsersPage() {
  const { api } = useAuth()
  const [users, setUsers] = useState<User[]>([])
  const [cursor, setCursor] = useState<string | undefined>()
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(
    async (after?: string) => {
      setLoading(true)
      setError(null)
      try {
        const q = new URLSearchParams({ limit: String(PAGE_SIZE) })
        if (after) q.set('cursor', after)
        const page = await api<Page>(`/api/v1/admin/users?${q}`)
        setUsers((prev) => (after ? [...prev, ...page.items] : page.items))
        setCursor(page.next_cursor)
      } catch (e) {
        setError(e instanceof ApiError ? e.message : 'Beklenmeyen hata')
      } finally {
        setLoading(false)
      }
    },
    [api],
  )

  useEffect(() => {
    void load()
  }, [load])

  return (
    <section className="card">
      <h2>Kullanıcılar</h2>
      {error && <ErrorBox message={error} onRetry={() => void load(cursor)} />}
      {!loading && !error && users.length === 0 && <Empty>Henüz kullanıcı yok.</Empty>}
      {users.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Ad</th>
                <th>E-posta</th>
                <th>Rol</th>
              </tr>
            </thead>
            <tbody>
              {users.map((u) => (
                <tr key={u.id}>
                  <td>{u.full_name}</td>
                  <td>{u.email}</td>
                  <td>{roleLabels[u.role]}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {loading && <Loading />}
      {!loading && cursor && (
        <button type="button" className="secondary" onClick={() => void load(cursor)}>
          Daha fazla yükle
        </button>
      )}
    </section>
  )
}
