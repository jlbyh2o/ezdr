import { ErrorAlert } from '@/components/error-alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import type { EnrollmentToken } from '@/gen/ezdr/portal/v1/portal_pb'
import { tokenClient } from '@/lib/api'
import { formatDateTime, toDate } from '@/lib/format'
import { usePoll } from '@/lib/use-poll'
import { PageHeader } from '@/pages/layout'

function tokenState(t: EnrollmentToken): 'active' | 'used' | 'revoked' | 'expired' {
  if (t.usedAt) return 'used'
  if (t.revokedAt) return 'revoked'
  const expires = toDate(t.expiresAt)
  if (expires && expires < new Date()) return 'expired'
  return 'active'
}

export function TokensPage() {
  const { data, error, reload } = usePoll(async () => (await tokenClient.listTokens({})).tokens, 30_000)

  async function revoke(id: string) {
    await tokenClient.revokeToken({ id })
    void reload()
  }

  return (
    <>
      <PageHeader
        title="Enrollment tokens"
        description="Single-use tokens created with “Add host”. Revoke any you no longer need."
      />
      <ErrorAlert message={error} />
      {data && data.length === 0 && <p className="py-12 text-center text-muted-foreground">No tokens yet.</p>}
      {data && data.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Description</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Created</TableHead>
              <TableHead>Expires</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {data.map((t) => {
              const state = tokenState(t)
              return (
                <TableRow key={t.id}>
                  <TableCell>{t.description || <span className="text-muted-foreground">—</span>}</TableCell>
                  <TableCell>
                    <Badge variant={state === 'active' ? 'success' : 'neutral'}>{state}</Badge>
                  </TableCell>
                  <TableCell className="text-xs">
                    {formatDateTime(t.createdAt)} by {t.createdBy}
                  </TableCell>
                  <TableCell className="text-xs">{formatDateTime(t.expiresAt)}</TableCell>
                  <TableCell className="text-right">
                    {state === 'active' && (
                      <Button variant="ghost" size="sm" onClick={() => void revoke(t.id)}>
                        Revoke
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      )}
    </>
  )
}
