import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { createApi, publicRequest } from '../api/client'
import type { User } from './roles'

type Session = { access_token: string; user: User }

type AuthState =
  | { status: 'loading' }
  | { status: 'anonymous' }
  | { status: 'authenticated'; user: User }

type AuthContextValue = {
  state: AuthState
  login: (email: string, password: string) => Promise<void>
  logout: () => Promise<void>
  api: ReturnType<typeof createApi>
}

const AuthContext = createContext<AuthContextValue | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>({ status: 'loading' })
  // Access token yalnızca bellekte tutulur; sayfa yenilenince çerezdeki
  // refresh token ile sessizce yeniden alınır. localStorage'a yazılmaz (XSS).
  const tokenRef = useRef<string | null>(null)
  const refreshing = useRef<Promise<string | null> | null>(null)

  const applySession = useCallback((s: Session) => {
    tokenRef.current = s.access_token
    setState({ status: 'authenticated', user: s.user })
  }, [])

  const refresh = useCallback((): Promise<string | null> => {
    // Aynı anda birden çok istek 401 alırsa tek yenileme yapılır; refresh
    // token tek kullanımlık olduğu için paralel yenileme oturumu düşürürdü.
    refreshing.current ??= publicRequest<Session>('/api/v1/auth/refresh', { method: 'POST' })
      .then((s) => {
        applySession(s)
        return s.access_token
      })
      .catch(() => {
        tokenRef.current = null
        setState({ status: 'anonymous' })
        return null
      })
      .finally(() => {
        refreshing.current = null
      })
    return refreshing.current
  }, [applySession])

  useEffect(() => {
    void refresh()
  }, [refresh])

  const api = useMemo(() => createApi({ getToken: () => tokenRef.current, refresh }), [refresh])

  const login = useCallback(
    async (email: string, password: string) => {
      applySession(await publicRequest<Session>('/api/v1/auth/login', { method: 'POST', body: { email, password } }))
    },
    [applySession],
  )

  const logout = useCallback(async () => {
    await publicRequest('/api/v1/auth/logout', { method: 'POST' }).catch(() => undefined)
    tokenRef.current = null
    setState({ status: 'anonymous' })
  }, [])

  const value = useMemo(() => ({ state, login, logout, api }), [state, login, logout, api])
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth AuthProvider içinde kullanılmalı')
  return ctx
}
