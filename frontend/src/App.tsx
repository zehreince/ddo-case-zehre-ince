import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom'
import { AuthProvider, useAuth } from './auth/AuthContext'
import { Layout } from './components/Layout'
import { RequireRole } from './components/RequireRole'
import { Loading } from './components/Status'
import { HomePage } from './pages/HomePage'
import { LoginPage } from './pages/LoginPage'
import { UsersPage } from './pages/UsersPage'

function AppRoutes() {
  const { state } = useAuth()

  if (state.status === 'loading') {
    return (
      <main className="shell">
        <Loading label="Oturum kontrol ediliyor…" />
      </main>
    )
  }
  if (state.status === 'anonymous') {
    return (
      <Routes>
        <Route path="*" element={<LoginPage />} />
      </Routes>
    )
  }
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route index element={<HomePage />} />
        <Route
          path="admin/users"
          element={
            <RequireRole roles={['admin']}>
              <UsersPage />
            </RequireRole>
          }
        />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  )
}

export function App() {
  return (
    <BrowserRouter>
      <AuthProvider>
        <AppRoutes />
      </AuthProvider>
    </BrowserRouter>
  )
}
