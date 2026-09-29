import { lazy, useEffect, useState } from 'react'
import { BrowserRouter, Navigate, Route, Routes } from 'react-router'

import { ErrorAlert } from '@/components/error-alert'
import type { User } from '@/gen/ezdr/portal/v1/portal_pb'
import { authClient, errorMessage, isUnauthenticated, setupClient } from '@/lib/api'
import { Layout } from '@/pages/layout'
import { LoginPage } from '@/pages/login'
import { SetupPage } from '@/pages/setup'

// Pages load on demand, keeping the first download small.
const AlertsPage = lazy(() => import('@/pages/alerts').then((m) => ({ default: m.AlertsPage })))
const AuditPage = lazy(() => import('@/pages/audit').then((m) => ({ default: m.AuditPage })))
const HostDetailPage = lazy(() => import('@/pages/host-detail').then((m) => ({ default: m.HostDetailPage })))
const HostsPage = lazy(() => import('@/pages/hosts').then((m) => ({ default: m.HostsPage })))
const OverviewPage = lazy(() => import('@/pages/overview').then((m) => ({ default: m.OverviewPage })))
const PlanEditorPage = lazy(() => import('@/pages/plan-editor').then((m) => ({ default: m.PlanEditorPage })))
const PlansPage = lazy(() => import('@/pages/plans').then((m) => ({ default: m.PlansPage })))
const SettingsPage = lazy(() => import('@/pages/settings').then((m) => ({ default: m.SettingsPage })))
const TokensPage = lazy(() => import('@/pages/tokens').then((m) => ({ default: m.TokensPage })))

type State =
  | { kind: 'loading' }
  | { kind: 'error'; message: string }
  | { kind: 'setup' }
  | { kind: 'signed-out' }
  | { kind: 'signed-in'; user: User }

async function loadState(): Promise<State> {
  try {
    const { setupRequired } = await setupClient.getSetupStatus({})
    if (setupRequired) return { kind: 'setup' }
    const { user } = await authClient.getCurrentUser({})
    return user ? { kind: 'signed-in', user } : { kind: 'signed-out' }
  } catch (err) {
    return isUnauthenticated(err) ? { kind: 'signed-out' } : { kind: 'error', message: errorMessage(err) }
  }
}

function App() {
  const [state, setState] = useState<State>({ kind: 'loading' })

  useEffect(() => {
    let active = true
    void loadState().then((s) => {
      if (active) setState(s)
    })
    return () => {
      active = false
    }
  }, [])

  async function signOut() {
    await authClient.logout({}).catch(() => undefined)
    setState({ kind: 'signed-out' })
  }

  switch (state.kind) {
    case 'loading':
      return null
    case 'error':
      return (
        <main className="mx-auto max-w-md p-8">
          <ErrorAlert message={`Cannot reach the portal: ${state.message}`} />
        </main>
      )
    case 'setup':
      return <SetupPage onDone={() => setState({ kind: 'signed-out' })} />
    case 'signed-out':
      return <LoginPage onSignedIn={(user) => setState({ kind: 'signed-in', user })} />
    case 'signed-in':
      return (
        <BrowserRouter>
          <Routes>
            <Route element={<Layout user={state.user} onSignOut={() => void signOut()} />}>
              <Route path="/" element={<OverviewPage />} />
              <Route path="/hosts" element={<HostsPage />} />
              <Route path="/hosts/:id" element={<HostDetailPage />} />
              <Route path="/plans" element={<PlansPage />} />
              <Route path="/plans/new" element={<PlanEditorPage />} />
              <Route path="/plans/:id" element={<PlanEditorPage key="edit" />} />
              <Route path="/tokens" element={<TokensPage />} />
              <Route path="/alerts" element={<AlertsPage />} />
              <Route path="/audit" element={<AuditPage />} />
              <Route path="/settings" element={<SettingsPage />} />
              <Route path="*" element={<Navigate to="/" replace />} />
            </Route>
          </Routes>
        </BrowserRouter>
      )
  }
}

export default App
