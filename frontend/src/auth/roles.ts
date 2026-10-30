export type Role = 'customer' | 'seller' | 'operations' | 'finance' | 'carrier' | 'admin'

export type User = {
  id: string
  email: string
  full_name: string
  role: Role
}

export const roleLabels: Record<Role, string> = {
  customer: 'Müşteri',
  seller: 'Satıcı',
  operations: 'Pazaryeri operasyon',
  finance: 'Finans',
  carrier: 'Kargo entegrasyonu',
  admin: 'Yönetici',
}

export type NavItem = { to: string; label: string; roles: Role[] | 'all' }

// Menü yalnızca rolün erişebildiği ekranları gösterir. Asıl engel sunucudadır
// (403); bu liste kullanıcıyı göremeyeceği sayfalara yönlendirmemek içindir.
export const navItems: NavItem[] = [
  { to: '/', label: 'Hesabım', roles: 'all' },
  { to: '/admin/users', label: 'Kullanıcılar', roles: ['admin'] },
]

export function canAccess(role: Role, allowed: Role[] | 'all'): boolean {
  return allowed === 'all' || allowed.includes(role)
}

export function navFor(role: Role): NavItem[] {
  return navItems.filter((i) => canAccess(role, i.roles))
}
