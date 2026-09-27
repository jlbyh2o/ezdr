import { clone, create } from '@bufbuild/protobuf'
import { Plus, Send, Trash2 } from 'lucide-react'
import { useEffect, useState } from 'react'

import { ErrorAlert } from '@/components/error-alert'
import { NativeSelect } from '@/components/native-select'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import {
  type AlertSettings,
  AlertSettingsSchema,
  SmtpSecurity,
  SmtpSettingsSchema,
  WebhookSchema,
} from '@/gen/ezdr/portal/v1/portal_pb'
import { alertClient, errorMessage } from '@/lib/api'
import { PageHeader } from '@/pages/layout'
import { Field } from '@/pages/setup'

export function SettingsPage() {
  const [settings, setSettings] = useState<AlertSettings>()
  const [error, setError] = useState<string>()
  const [notice, setNotice] = useState<string[]>()
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    void alertClient.getAlertSettings({}).then(
      (r) => setSettings(r.settings ?? create(AlertSettingsSchema, { smtp: create(SmtpSettingsSchema) })),
      (e) => setError(errorMessage(e)),
    )
  }, [])

  function update(fn: (s: AlertSettings) => void) {
    setSettings((prev) => {
      if (!prev) return prev
      const next = clone(AlertSettingsSchema, prev)
      if (!next.smtp) next.smtp = create(SmtpSettingsSchema)
      fn(next)
      return next
    })
  }

  async function save() {
    if (!settings) return
    setBusy(true)
    setError(undefined)
    setNotice(undefined)
    try {
      const r = await alertClient.updateAlertSettings({ settings })
      setSettings(r.settings)
      setNotice(['Saved.'])
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  async function test() {
    setBusy(true)
    setError(undefined)
    try {
      const r = await alertClient.sendTestAlert({})
      setNotice(r.results)
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  if (!settings) return <ErrorAlert message={error} />
  const smtp = settings.smtp ?? create(SmtpSettingsSchema)
  return (
    <>
      <PageHeader title="Settings" description="Where alerts are sent. Passwords and secrets are stored encrypted and never shown again.">
        <div className="flex gap-2">
          <Button variant="outline" onClick={() => void test()} disabled={busy}>
            <Send /> Send test
          </Button>
          <Button onClick={() => void save()} disabled={busy}>
            Save
          </Button>
        </div>
      </PageHeader>
      <ErrorAlert message={error} />
      {notice && (
        <ul className="rounded-lg border bg-muted/50 p-3 text-sm">
          {notice.map((n) => (
            <li key={n}>{n}</li>
          ))}
        </ul>
      )}
      <Card>
        <CardHeader>
          <CardTitle>Email</CardTitle>
          <CardDescription>Send alerts through an SMTP server.</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={smtp.enabled} onChange={(e) => update((s) => (s.smtp!.enabled = e.target.checked))} />
            Send email alerts
          </label>
          <div className="grid gap-4 sm:grid-cols-[1fr_100px_160px]">
            <Field id="smtp-host" label="SMTP server">
              <Input id="smtp-host" value={smtp.host} placeholder="smtp.example.com" onChange={(e) => update((s) => (s.smtp!.host = e.target.value))} />
            </Field>
            <Field id="smtp-port" label="Port">
              <Input
                id="smtp-port"
                type="number"
                value={smtp.port || ''}
                onChange={(e) => update((s) => (s.smtp!.port = Math.round(Number(e.target.value))))}
              />
            </Field>
            <Field id="smtp-security" label="Security">
              <NativeSelect
                id="smtp-security"
                value={smtp.security}
                onChange={(e) => update((s) => (s.smtp!.security = Number(e.target.value)))}
              >
                <option value={SmtpSecurity.STARTTLS}>STARTTLS</option>
                <option value={SmtpSecurity.TLS}>TLS</option>
                <option value={SmtpSecurity.NONE}>None (local relay only)</option>
              </NativeSelect>
            </Field>
          </div>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field id="smtp-user" label="Username (optional)">
              <Input id="smtp-user" value={smtp.username} autoComplete="off" onChange={(e) => update((s) => (s.smtp!.username = e.target.value))} />
            </Field>
            <Field id="smtp-pass" label={smtp.hasPassword ? 'Password (leave empty to keep)' : 'Password'}>
              <Input
                id="smtp-pass"
                type="password"
                autoComplete="new-password"
                value={smtp.password}
                onChange={(e) => update((s) => (s.smtp!.password = e.target.value))}
              />
            </Field>
            <Field id="smtp-from" label="Sender">
              <Input id="smtp-from" value={smtp.from} placeholder="EZDR <ezdr@example.com>" onChange={(e) => update((s) => (s.smtp!.from = e.target.value))} />
            </Field>
            <Field id="smtp-to" label="Recipients (comma-separated)">
              <Input
                id="smtp-to"
                value={smtp.recipients.join(', ')}
                onChange={(e) =>
                  update(
                    (s) =>
                      (s.smtp!.recipients = e.target.value
                        .split(',')
                        .map((x) => x.trim())
                        .filter(Boolean)),
                  )
                }
              />
            </Field>
          </div>
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Webhooks</CardTitle>
          <CardDescription>
            Alerts are posted as JSON. With a secret, requests carry an X-EZDR-Signature header (HMAC-SHA256 of the body).
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-3">
          {settings.webhooks.map((w, i) => (
            <div key={i} className="grid items-center gap-2 sm:grid-cols-[1fr_220px_32px]">
              <Input
                aria-label="Webhook URL"
                placeholder="https://hooks.example.com/…"
                value={w.url}
                onChange={(e) => update((s) => (s.webhooks[i].url = e.target.value))}
              />
              <Input
                aria-label="Webhook secret"
                type="password"
                autoComplete="new-password"
                placeholder={w.hasSecret ? 'secret set (leave empty to keep)' : 'secret (optional)'}
                value={w.secret}
                onChange={(e) => update((s) => (s.webhooks[i].secret = e.target.value))}
              />
              <Button variant="ghost" size="icon-sm" aria-label="Remove webhook" onClick={() => update((s) => s.webhooks.splice(i, 1))}>
                <Trash2 />
              </Button>
            </div>
          ))}
          <div>
            <Button variant="outline" size="sm" onClick={() => update((s) => s.webhooks.push(create(WebhookSchema)))}>
              <Plus /> Add webhook
            </Button>
          </div>
        </CardContent>
      </Card>
    </>
  )
}
