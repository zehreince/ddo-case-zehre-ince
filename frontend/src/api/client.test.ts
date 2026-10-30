import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError, createApi, parseResponse } from './client'

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

describe('parseResponse', () => {
  it('başarılı gövdeyi döner', async () => {
    await expect(parseResponse(json(200, { status: 'ok' }))).resolves.toEqual({ status: 'ok' })
  })

  it('204 için gövde okumaz', async () => {
    await expect(parseResponse(new Response(null, { status: 204 }))).resolves.toBeUndefined()
  })

  it('standart hata gövdesini ApiError yapar', async () => {
    const err = await parseResponse(json(403, { error: { code: 'forbidden', message: 'yetkin yok' } })).catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({ status: 403, code: 'forbidden', message: 'yetkin yok' })
  })

  it('doğrulama hatasında alanları taşır', async () => {
    const body = { error: { code: 'validation_failed', message: 'x', fields: { email: 'zorunlu' } } }
    const err = await parseResponse(json(422, body)).catch((e: ApiError) => e)
    expect(err).toMatchObject({ fields: { email: 'zorunlu' } })
  })

  it('JSON olmayan hatada da anlamlı mesaj üretir', async () => {
    const err = await parseResponse(new Response('bad gateway', { status: 502 })).catch((e) => e)
    expect(err).toMatchObject({ status: 502, code: 'unknown', message: 'HTTP 502' })
  })
})

describe('createApi', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('access token ekler', async () => {
    const fetchMock = vi.fn().mockResolvedValue(json(200, { ok: true }))
    vi.stubGlobal('fetch', fetchMock)
    const api = createApi({ getToken: () => 'tok', refresh: vi.fn() })

    await api('/x')
    expect(fetchMock.mock.calls[0][1].headers.Authorization).toBe('Bearer tok')
  })

  it('401 alınca bir kez yeniler ve isteği yeni token ile tekrarlar', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(json(401, { error: { code: 'unauthorized', message: 'süre doldu' } }))
      .mockResolvedValueOnce(json(200, { ok: true }))
    vi.stubGlobal('fetch', fetchMock)
    const refresh = vi.fn().mockResolvedValue('yeni')
    const api = createApi({ getToken: () => 'eski', refresh })

    await expect(api('/x')).resolves.toEqual({ ok: true })
    expect(refresh).toHaveBeenCalledOnce()
    expect(fetchMock.mock.calls[1][1].headers.Authorization).toBe('Bearer yeni')
  })

  it('yenileme başarısızsa 401 hatasını iletir', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(401, { error: { code: 'unauthorized', message: 'x' } })))
    const api = createApi({ getToken: () => 'eski', refresh: vi.fn().mockResolvedValue(null) })

    await expect(api('/x')).rejects.toMatchObject({ status: 401 })
  })

  it('403 için yenileme denemez', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(403, { error: { code: 'forbidden', message: 'x' } })))
    const refresh = vi.fn()
    const api = createApi({ getToken: () => 'tok', refresh })

    await expect(api('/x')).rejects.toMatchObject({ status: 403 })
    expect(refresh).not.toHaveBeenCalled()
  })

  it('ağ hatasını network_error yapar', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))
    const api = createApi({ getToken: () => null, refresh: vi.fn() })

    await expect(api('/x')).rejects.toMatchObject({ status: 0, code: 'network_error' })
  })
})
