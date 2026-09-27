import { useEffect, useState } from 'react'
import { BrowserRouter, Navigate, Route, Routes } from 'react-router'

import { ErrorAlert } from '@/components/error-alert'
import type { User } from '@/gen/ezdr/portal/v1/portal_pb'
import { authClient, errorMessage, isUnauthenticated, setupClient } from '@/lib/api'
import { AuditPage } from '@/pages/audit'
import { HostDetailPage } from '@/pages/host-detail'
import { HostsPage } from '@/pages/hosts'
import { Layout } from '@/pages/layout'
import { LoginPage } from '@/pages/login'
import { SetupPage } from '@/pages/setup'
import { TokensPage } from '@/pages/tokens'

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
              <Route path="/hosts" element={<HostsPage />} />
              <Route path="/hosts/:id" element={<HostDetailPage />} />
              <Route path="/tokens" element={<TokensPage />} />
              <Route path="/audit" element={<AuditPage />} />
              <Route path="*" element={<Navigate to="/hosts" replace />} />
            </Route>
          </Routes>
        </BrowserRouter>
      )
  }
}

export default App
