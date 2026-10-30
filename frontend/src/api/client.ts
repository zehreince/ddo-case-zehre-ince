// API istemcisi. Backend her hatayı {"error":{"code","message","fields?"}}
// biçiminde döner; burada tek tip ApiError'a çevrilir ki ekranlar tek yoldan ele alsın.

export const BASE_URL = import.meta.env.VITE_API_URL ?? 'http://localhost:8080'

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    public readonly code: string,
    message: string,
    public readonly fields: Record<string, string> = {},
  ) {
    super(message)
  }
}

export async function parseResponse<T>(res: Response): Promise<T> {
  if (res.status === 204) return undefined as T
  const body = await res.json().catch(() => null)
  if (!res.ok) {
    const err = body?.error
    throw new ApiError(res.status, err?.code ?? 'unknown', err?.message ?? `HTTP ${res.status}`, err?.fields ?? {})
  }
  return body as T
}

type RequestOptions = {
  method?: string
  body?: unknown
  signal?: AbortSignal
}

async function send(path: string, opts: RequestOptions, token: string | null): Promise<Response> {
  const headers: Record<string, string> = {}
  if (opts.body !== undefined) headers['Content-Type'] = 'application/json'
  if (token) headers['Authorization'] = `Bearer ${token}`
  try {
    return await fetch(BASE_URL + path, {
      method: opts.method ?? 'GET',
      headers,
      body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
      signal: opts.signal,
      // refresh token httpOnly çerezde; yalnızca /auth uçlarına gider (Path=/api/v1/auth).
      credentials: 'include',
    })
  } catch (e) {
    if (e instanceof DOMException && e.name === 'AbortError') throw e
    throw new ApiError(0, 'network_error', 'Sunucuya ulaşılamadı')
  }
}

export type TokenSource = {
  getToken: () => string | null
  // Access token süresi dolduğunda sessizce yeniler; başarısızsa null.
  refresh: () => Promise<string | null>
}

// createApi access token'ı ekler; 401 gelirse bir kez yenileyip isteği tekrarlar.
export function createApi(tokens: TokenSource) {
  return async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
    let res = await send(path, opts, tokens.getToken())
    if (res.status === 401 && tokens.getToken()) {
      const fresh = await tokens.refresh()
      if (fresh) res = await send(path, opts, fresh)
    }
    return parseResponse<T>(res)
  }
}

// publicRequest oturum gerektirmeyen uçlar içindir (health, login, refresh).
export async function publicRequest<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  return parseResponse<T>(await send(path, opts, null))
}
