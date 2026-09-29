import { useState } from 'react'

import { CopyField } from '@/components/copy-field'
import { ErrorAlert } from '@/components/error-alert'
import { QrCode } from '@/components/qr-code'
import { Button } from '@/components/ui/button'
import { CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import type { LoginResponse, User } from '@/gen/ezdr/portal/v1/portal_pb'
import { authClient, errorMessage } from '@/lib/api'
import { CenteredCard, Field } from '@/pages/setup'

type Step =
  | { kind: 'password' }
  | { kind: 'code'; login: LoginResponse }
  | { kind: 'recovery'; user: User; codes: string[] }

export function LoginPage({ onSignedIn }: { onSignedIn: (u: User) => void }) {
  const [step, setStep] = useState<Step>({ kind: 'password' })
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [code, setCode] = useState('')
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)

  async function run(fn: () => Promise<void>) {
    setBusy(true)
    setError(undefined)
    try {
      await fn()
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  function submitPassword(e: React.FormEvent) {
    e.preventDefault()
    void run(async () => {
      const login = await authClient.login({ username, password })
      setPassword('')
      setStep({ kind: 'code', login })
    })
  }

  function submitCode(e: React.FormEvent) {
    e.preventDefault()
    if (step.kind !== 'code') return
    void run(async () => {
      const res = await authClient.verifyTotp({ challengeId: step.login.challengeId, code })
      if (res.recoveryCodes.length > 0 && res.user) {
        setStep({ kind: 'recovery', user: res.user, codes: res.recoveryCodes })
      } else if (res.user) {
        onSignedIn(res.user)
      }
    })
  }

  if (step.kind === 'recovery') {
    return (
      <CenteredCard>
        <CardHeader>
          <CardTitle>Save your recovery codes</CardTitle>
          <CardDescription>
            Each code signs you in once if you lose your authenticator app. They won&apos;t be shown again.
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          <ul className="grid grid-cols-2 gap-2 font-mono text-sm">
            {step.codes.map((c) => (
              <li key={c} className="rounded-md border bg-muted px-3 py-1.5 text-center">
                {c}
              </li>
            ))}
          </ul>
          <CopyField label="recovery codes" value={step.codes.join('\n')} />
          <Button onClick={() => onSignedIn(step.user)}>I&apos;ve saved these codes</Button>
        </CardContent>
      </CenteredCard>
    )
  }

  if (step.kind === 'code') {
    const setup = step.login.totpSetup
    return (
      <CenteredCard>
        <CardHeader>
          <CardTitle>{setup ? 'Set up two-factor authentication' : 'Two-factor authentication'}</CardTitle>
          <CardDescription>
            {setup
              ? 'Scan the QR code with an authenticator app, then enter the 6-digit code it shows.'
              : 'Enter the 6-digit code from your authenticator app, or a recovery code.'}
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form key="code" onSubmit={submitCode} className="grid gap-4">
            <ErrorAlert message={error} />
            {setup && (
              <div className="grid justify-items-center gap-3">
                <QrCode value={setup.url} />
                <p className="text-sm text-muted-foreground">Can&apos;t scan it? Enter this key instead:</p>
                <div className="w-full">
                  <CopyField label="setup key" value={setup.secret} />
                </div>
              </div>
            )}
            <Field id="totp" label={setup ? '6-digit code' : 'Code'}>
              <Input
                id="totp"
                name="totp"
                value={code}
                onChange={(e) => setCode(e.target.value)}
                inputMode={setup ? 'numeric' : 'text'}
                autoComplete="one-time-code"
                autoFocus
                required
              />
            </Field>
            <Button type="submit" disabled={busy}>
              Verify
            </Button>
            <Button type="button" variant="ghost" onClick={() => setStep({ kind: 'password' })}>
              Start over
            </Button>
          </form>
        </CardContent>
      </CenteredCard>
    )
  }

  return (
    <CenteredCard>
      <CardHeader>
        <CardTitle>Sign in</CardTitle>
      </CardHeader>
      <CardContent>
        <form key="password" onSubmit={submitPassword} className="grid gap-4">
          <ErrorAlert message={error} />
          <Field id="username" label="Username">
            <Input id="username" name="username" value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" autoFocus required />
          </Field>
          <Field id="password" label="Password">
            <Input id="password" name="password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" required />
          </Field>
          <Button type="submit" disabled={busy}>
            Continue
          </Button>
        </form>
      </CardContent>
    </CenteredCard>
  )
}
