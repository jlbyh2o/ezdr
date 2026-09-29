import { useState } from 'react'

import { ErrorAlert } from '@/components/error-alert'
import { LogoMark } from '@/components/logo'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { errorMessage, setupClient } from '@/lib/api'

export function SetupPage({ onDone }: { onDone: () => void }) {
  const [code, setCode] = useState('')
  const [username, setUsername] = useState('admin')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (password !== confirm) {
      setError('Passwords do not match.')
      return
    }
    setBusy(true)
    try {
      await setupClient.completeSetup({ setupCode: code, username, password })
      onDone()
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <CenteredCard>
      <CardHeader>
        <CardTitle>Welcome to EZDR</CardTitle>
        <CardDescription>
          Create the first administrator. The setup code is printed in the portal&apos;s log.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form onSubmit={submit} className="grid gap-4">
          <ErrorAlert message={error} />
          <Field id="code" label="Setup code">
            <Input id="code" value={code} onChange={(e) => setCode(e.target.value)} placeholder="xxxx-xxxx-xxxx-xxxx" autoComplete="off" required />
          </Field>
          <Field id="username" label="Username">
            <Input id="username" value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" required />
          </Field>
          <Field id="password" label="Password (at least 12 characters)">
            <Input id="password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" minLength={12} required />
          </Field>
          <Field id="confirm" label="Confirm password">
            <Input id="confirm" type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="new-password" required />
          </Field>
          <Button type="submit" disabled={busy}>
            Create administrator
          </Button>
        </form>
      </CardContent>
    </CenteredCard>
  )
}

// CenteredCard frames the screens before sign-in: setup, sign-in, and
// two-factor authentication.
export function CenteredCard({ children }: { children: React.ReactNode }) {
  return (
    <main className="relative flex min-h-svh flex-col items-center justify-center gap-6 overflow-hidden p-4">
      <div
        aria-hidden
        className="pointer-events-none absolute inset-x-0 top-0 h-80 bg-gradient-to-b from-primary/10 to-transparent dark:from-primary/15"
      />
      <div className="relative flex items-center gap-3">
        <LogoMark className="size-10" />
        <div className="grid leading-tight">
          <span className="text-xl font-semibold tracking-tight">EZDR</span>
          <span className="text-sm text-muted-foreground">Disaster recovery for Proxmox VE</span>
        </div>
      </div>
      <Card className="relative w-full max-w-md shadow-lg">{children}</Card>
    </main>
  )
}

export function Field({ id, label, children }: { id: string; label: string; children: React.ReactNode }) {
  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{label}</Label>
      {children}
    </div>
  )
}
