import { useState } from 'react'

import { ErrorAlert } from '@/components/error-alert'
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

export function CenteredCard({ children }: { children: React.ReactNode }) {
  return (
    <main className="flex min-h-svh items-center justify-center p-4">
      <Card className="w-full max-w-md">{children}</Card>
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
