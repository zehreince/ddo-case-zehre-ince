import { NavLink, Outlet } from 'react-router-dom'
import { useAuth } from '../auth/AuthContext'
import { navFor, roleLabels } from '../auth/roles'

export function Layout() {
  const { state, logout } = useAuth()
  if (state.status !== 'authenticated') return null
  const { user } = state

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">ÇarşıHub</div>
        <nav className="nav" aria-label="Ana menü">
          {navFor(user.role).map((item) => (
            <NavLink key={item.to} to={item.to} end className={({ isActive }) => (isActive ? 'active' : undefined)}>
              {item.label}
            </NavLink>
          ))}
        </nav>
        <div className="who">
          <span className="who-name">{user.full_name}</span>
          <span className="badge">{roleLabels[user.role]}</span>
          <button type="button" className="link" onClick={() => void logout()}>
            Çıkış
          </button>
        </div>
      </header>
      <main className="shell">
        <Outlet />
      </main>
    </div>
  )
}
