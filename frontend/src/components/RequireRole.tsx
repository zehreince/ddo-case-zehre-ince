import type { ReactNode } from 'react'
import { useAuth } from '../auth/AuthContext'
import { canAccess, type Role } from '../auth/roles'

// Kullanıcı rolü uygun değilse sayfayı hiç yüklemez. Sunucu zaten 403 döner;
// bu yalnızca adres çubuğundan girilen bağlantılarda anlaşılır bir ekran içindir.
export function RequireRole({ roles, children }: { roles: Role[]; children: ReactNode }) {
  const { state } = useAuth()
  if (state.status !== 'authenticated') return null
  if (!canAccess(state.user.role, roles)) {
    return (
      <section className="card">
        <h2>Erişim yok</h2>
        <p className="muted">Bu sayfayı görüntülemek için yetkiniz bulunmuyor.</p>
      </section>
    )
  }
  return <>{children}</>
}
