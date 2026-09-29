import { Bell, ChevronsUpDown, LayoutDashboard, LogOut, Monitor, Moon, ScrollText, Server, Settings, ShieldCheck, Sun } from 'lucide-react'
import { Link, Outlet, useLocation } from 'react-router'

import { LogoMark } from '@/components/logo'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Separator } from '@/components/ui/separator'
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarInset,
  SidebarMenu,
  SidebarMenuBadge,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarProvider,
  SidebarRail,
  SidebarTrigger,
} from '@/components/ui/sidebar'
import type { User } from '@/gen/ezdr/portal/v1/portal_pb'
import { alertClient } from '@/lib/api'
import { type Theme, useTheme } from '@/lib/theme'
import { usePoll } from '@/lib/use-poll'

type NavItem = { to: string; label: string; icon: React.ComponentType }

const groups: { label: string; items: NavItem[] }[] = [
  { label: 'Site', items: [{ to: '/', label: 'Overview', icon: LayoutDashboard }] },
  {
    label: 'Protection',
    items: [
      { to: '/plans', label: 'DR plans', icon: ShieldCheck },
      { to: '/hosts', label: 'Hosts', icon: Server },
    ],
  },
  {
    label: 'Activity',
    items: [
      { to: '/alerts', label: 'Alerts', icon: Bell },
      { to: '/audit', label: 'Audit log', icon: ScrollText },
    ],
  },
]

// Pages that belong to a navigation item without being under its path.
const aliases: Record<string, string> = { '/tokens': '/hosts' }

const allItems = [...groups.flatMap((g) => g.items), { to: '/settings', label: 'Settings', icon: Settings }]

function useActive() {
  const { pathname } = useLocation()
  const path = aliases[pathname] ?? pathname
  return (to: string) => path === to || (to !== '/' && path.startsWith(to + '/'))
}

export function Layout({ user, onSignOut }: { user: User; onSignOut: () => void }) {
  const isActive = useActive()
  const current = allItems.find((i) => isActive(i.to))
  return (
    <SidebarProvider>
      <AppSidebar user={user} onSignOut={onSignOut} />
      <SidebarInset>
        <header className="sticky top-0 z-10 flex h-12 shrink-0 items-center gap-2 border-b bg-background/90 px-4 backdrop-blur">
          <SidebarTrigger className="-ml-1" />
          <Separator orientation="vertical" className="mr-1 data-[orientation=vertical]:h-4" />
          <span className="text-sm text-muted-foreground">{current?.label}</span>
        </header>
        <div className="mx-auto grid w-full max-w-7xl gap-4 p-4 md:p-6">
          <Outlet />
        </div>
      </SidebarInset>
    </SidebarProvider>
  )
}

function AppSidebar({ user, onSignOut }: { user: User; onSignOut: () => void }) {
  const isActive = useActive()
  const { data: alerts } = usePoll(() => alertClient.listAlerts({}), 30000)
  const firing = alerts?.alerts.filter((a) => !a.resolvedAt).length ?? 0

  return (
    <Sidebar collapsible="icon">
      <SidebarHeader>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton size="lg" render={<Link to="/" />}>
              <LogoMark className="size-8! shrink-0" />
              <div className="grid leading-tight">
                <span className="font-semibold">EZDR</span>
                <span className="text-xs text-sidebar-foreground/60">Proxmox VE disaster recovery</span>
              </div>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>
      <SidebarContent>
        {groups.map((g) => (
          <SidebarGroup key={g.label}>
            <SidebarGroupLabel>{g.label}</SidebarGroupLabel>
            <SidebarMenu>
              {g.items.map((item) => (
                <SidebarMenuItem key={item.to}>
                  <SidebarMenuButton isActive={isActive(item.to)} tooltip={item.label} render={<Link to={item.to} />}>
                    <item.icon />
                    <span>{item.label}</span>
                  </SidebarMenuButton>
                  {item.to === '/alerts' && firing > 0 && (
                    <SidebarMenuBadge className="bg-destructive text-white">{firing}</SidebarMenuBadge>
                  )}
                </SidebarMenuItem>
              ))}
            </SidebarMenu>
          </SidebarGroup>
        ))}
      </SidebarContent>
      <SidebarFooter>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton isActive={isActive('/settings')} tooltip="Settings" render={<Link to="/settings" />}>
              <Settings />
              <span>Settings</span>
            </SidebarMenuButton>
          </SidebarMenuItem>
          <SidebarMenuItem>
            <UserMenu user={user} onSignOut={onSignOut} />
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarFooter>
      <SidebarRail />
    </Sidebar>
  )
}

const themes: { value: Theme; label: string; icon: React.ComponentType }[] = [
  { value: 'light', label: 'Light', icon: Sun },
  { value: 'dark', label: 'Dark', icon: Moon },
  { value: 'system', label: 'System', icon: Monitor },
]

function UserMenu({ user, onSignOut }: { user: User; onSignOut: () => void }) {
  const [theme, setTheme] = useTheme()
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <SidebarMenuButton size="lg" className="data-popup-open:bg-sidebar-accent">
            <span className="flex size-8 shrink-0 items-center justify-center rounded-md bg-sidebar-accent text-sm font-medium uppercase">
              {user.username.slice(0, 1)}
            </span>
            <span className="truncate">{user.username}</span>
            <ChevronsUpDown className="ml-auto" />
          </SidebarMenuButton>
        }
      />
      <DropdownMenuContent side="right" align="end" className="min-w-52">
        <DropdownMenuGroup>
          <DropdownMenuLabel>Theme</DropdownMenuLabel>
          <DropdownMenuRadioGroup value={theme} onValueChange={(v) => setTheme(v as Theme)}>
            {themes.map((t) => (
              <DropdownMenuRadioItem key={t.value} value={t.value}>
                <t.icon /> {t.label}
              </DropdownMenuRadioItem>
            ))}
          </DropdownMenuRadioGroup>
        </DropdownMenuGroup>
        <DropdownMenuSeparator />
        <DropdownMenuItem onClick={onSignOut}>
          <LogOut /> Sign out
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
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
    <div className="flex flex-wrap items-end justify-between gap-4">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
        {description && <p className="text-sm text-muted-foreground">{description}</p>}
      </div>
      {children && <div className="flex items-center gap-2">{children}</div>}
    </div>
  )
}
