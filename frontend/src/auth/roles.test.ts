import { describe, expect, it } from 'vitest'
import { canAccess, navFor, roleLabels, type Role } from './roles'

const roles = Object.keys(roleLabels) as Role[]

describe('rol bazlı menü', () => {
  it('her rol kendi hesabını görür', () => {
    for (const r of roles) expect(navFor(r).map((i) => i.to)).toContain('/')
  })

  it('kullanıcı listesi yalnızca yöneticiye görünür', () => {
    for (const r of roles) {
      const visible = navFor(r).some((i) => i.to === '/admin/users')
      expect(visible).toBe(r === 'admin')
    }
  })

  it('canAccess', () => {
    expect(canAccess('seller', 'all')).toBe(true)
    expect(canAccess('seller', ['admin'])).toBe(false)
    expect(canAccess('admin', ['finance', 'admin'])).toBe(true)
  })
})
