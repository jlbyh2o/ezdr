import { LogOut } from 'lucide-react'
import { NavLink, Outlet } from 'react-router'

import { Button } from '@/components/ui/button'
import type { User } from '@/gen/ezdr/portal/v1/portal_pb'

const nav = [
  { to: '/hosts', label: 'Hosts' },
  { to: '/plans', label: 'DR plans' },
  { to: '/tokens', label: 'Enrollment tokens' },
  { to: '/audit', label: 'Audit log' },
]

export function Layout({ user, onSignOut }: { user: User; onSignOut: () => void }) {
  return (
    <div className="min-h-svh">
      <header className="border-b">
        <div className="mx-auto flex h-14 max-w-6xl items-center gap-6 px-4">
          <span className="font-semibold">EZDR</span>
          <nav className="flex gap-1">
            {nav.map((n) => (
              <NavLink
                key={n.to}
                to={n.to}
                className={({ isActive }) =>
                  `rounded-md px-3 py-1.5 text-sm ${isActive ? 'bg-muted font-medium' : 'text-muted-foreground hover:text-foreground'}`
                }
              >
                {n.label}
              </NavLink>
            ))}
          </nav>
          <div className="ml-auto flex items-center gap-2 text-sm text-muted-foreground">
            {user.username}
            <Button variant="ghost" size="sm" onClick={onSignOut}>
              <LogOut /> Sign out
            </Button>
          </div>
        </div>
      </header>
      <main className="mx-auto grid max-w-6xl gap-4 p-4">
        <Outlet />
      </main>
    </div>
  )
}

export function PageHeader({
  title,
  description,
  children,
}: {
  title: string
  description?: string
  children?: React.ReactNode
}) {
  return (
    <div className="flex items-end justify-between gap-4 pt-4">
      <div>
        <h1 className="text-2xl font-semibold">{title}</h1>
        {description && <p className="text-sm text-muted-foreground">{description}</p>}
      </div>
      {children}
    </div>
  )
}
