// API istemcisi. Backend her hatayı {"error":{"code","message"}} biçiminde döner;
// burada tek tip ApiError'a çevrilir ki ekranlar tek yoldan ele alsın.

const BASE_URL = import.meta.env.VITE_API_URL ?? 'http://localhost:8080'

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    public readonly code: string,
    message: string,
  ) {
    super(message)
  }
}

export async function parseResponse<T>(res: Response): Promise<T> {
  const body = await res.json().catch(() => null)
  if (!res.ok) {
    const err = body?.error
    throw new ApiError(res.status, err?.code ?? 'unknown', err?.message ?? `HTTP ${res.status}`)
  }
  return body as T
}

export async function apiGet<T>(path: string, signal?: AbortSignal): Promise<T> {
  let res: Response
  try {
    res = await fetch(BASE_URL + path, { signal })
  } catch (e) {
    if (e instanceof DOMException && e.name === 'AbortError') throw e
    throw new ApiError(0, 'network_error', 'Sunucuya ulaşılamadı')
  }
  return parseResponse<T>(res)
}
