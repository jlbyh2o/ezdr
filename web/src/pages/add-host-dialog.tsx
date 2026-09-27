import { Plus } from 'lucide-react'
import { useState } from 'react'

import { CopyField } from '@/components/copy-field'
import { ErrorAlert } from '@/components/error-alert'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import type { CreateTokenResponse } from '@/gen/ezdr/portal/v1/portal_pb'
import { errorMessage, tokenClient } from '@/lib/api'
import { formatDateTime } from '@/lib/format'
import { Field } from '@/pages/setup'

const lifetimes = [
  { label: '1 hour', seconds: 3600 },
  { label: '24 hours', seconds: 86400 },
  { label: '7 days', seconds: 604800 },
]

export function AddHostDialog({ onCreated }: { onCreated: () => void }) {
  const [open, setOpen] = useState(false)
  const [description, setDescription] = useState('')
  const [ttl, setTtl] = useState(lifetimes[0].seconds)
  const [created, setCreated] = useState<CreateTokenResponse>()
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)

  function reset() {
    setDescription('')
    setTtl(lifetimes[0].seconds)
    setCreated(undefined)
    setError(undefined)
  }

  async function create(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    try {
      setCreated(await tokenClient.createToken({ description, ttlSeconds: ttl }))
      setError(undefined)
      onCreated()
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o)
        if (!o) reset()
      }}
    >
      <DialogTrigger render={<Button />}>
        <Plus /> Add host
      </DialogTrigger>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Add a Proxmox VE host</DialogTitle>
          <DialogDescription>
            {created
              ? 'Run one of these commands as root on the host.'
              : 'Create a single-use enrollment token for one host.'}
          </DialogDescription>
        </DialogHeader>

        {!created ? (
          <form onSubmit={create} className="grid gap-4">
            <ErrorAlert message={error} />
            <Field id="description" label="Description (optional)">
              <Input
                id="description"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                placeholder="for example, primary site"
                maxLength={200}
              />
            </Field>
            <div className="grid gap-2">
              <Label>Token expires after</Label>
              <div className="flex gap-2">
                {lifetimes.map((l) => (
                  <Button
                    key={l.seconds}
                    type="button"
                    variant={ttl === l.seconds ? 'default' : 'outline'}
                    onClick={() => setTtl(l.seconds)}
                  >
                    {l.label}
                  </Button>
                ))}
              </div>
            </div>
            <DialogFooter>
              <Button type="submit" disabled={busy}>
                Create token
              </Button>
            </DialogFooter>
          </form>
        ) : (
          <div className="grid gap-4">
            <Tabs defaultValue="install">
              <TabsList>
                <TabsTrigger value="install">New host</TabsTrigger>
                <TabsTrigger value="enroll">Client already installed</TabsTrigger>
              </TabsList>
              <TabsContent value="install" className="grid gap-2 pt-2">
                <p className="text-sm text-muted-foreground">Installs the EZDR client, then enrolls the host.</p>
                <CopyField label="install command" value={created.installCommand} />
              </TabsContent>
              <TabsContent value="enroll" className="grid gap-2 pt-2">
                <p className="text-sm text-muted-foreground">For hosts where the ezdr client is already installed.</p>
                <CopyField label="enroll command" value={created.enrollCommand} />
              </TabsContent>
            </Tabs>
            <p className="text-sm text-muted-foreground">
              This token is shown only once, works for one host, and expires {formatDateTime(created.token?.expiresAt)}.
              The command lists the changes it will make and asks for confirmation before changing anything.
            </p>
            <DialogFooter showCloseButton />
          </div>
        )}
      </DialogContent>
    </Dialog>
  )
}
