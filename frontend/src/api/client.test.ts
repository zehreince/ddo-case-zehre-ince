import { describe, expect, it } from 'vitest'
import { ApiError, parseResponse } from './client'

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

describe('parseResponse', () => {
  it('başarılı gövdeyi döner', async () => {
    await expect(parseResponse(json(200, { status: 'ok' }))).resolves.toEqual({ status: 'ok' })
  })

  it('standart hata gövdesini ApiError yapar', async () => {
    const err = await parseResponse(json(403, { error: { code: 'forbidden', message: 'yetkin yok' } })).catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({ status: 403, code: 'forbidden', message: 'yetkin yok' })
  })

  it('JSON olmayan hatada da anlamlı mesaj üretir', async () => {
    const err = await parseResponse(new Response('bad gateway', { status: 502 })).catch((e) => e)
    expect(err).toMatchObject({ status: 502, code: 'unknown', message: 'HTTP 502' })
  })
})
