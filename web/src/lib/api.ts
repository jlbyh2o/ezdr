import { Code, ConnectError, createClient } from '@connectrpc/connect'
import { createConnectTransport } from '@connectrpc/connect-web'

import {
  AlertService,
  AuditService,
  AuthService,
  DnsService,
  FailoverService,
  HostService,
  PlanService,
  SetupService,
  TestFailoverService,
  TokenService,
} from '@/gen/ezdr/portal/v1/portal_pb'

// Same-origin requests; the session cookie is sent automatically.
const transport = createConnectTransport({ baseUrl: window.location.origin })

export const setupClient = createClient(SetupService, transport)
export const authClient = createClient(AuthService, transport)
export const hostClient = createClient(HostService, transport)
export const tokenClient = createClient(TokenService, transport)
export const auditClient = createClient(AuditService, transport)
export const planClient = createClient(PlanService, transport)
export const alertClient = createClient(AlertService, transport)
export const testClient = createClient(TestFailoverService, transport)
export const failoverClient = createClient(FailoverService, transport)
export const dnsClient = createClient(DnsService, transport)

export function errorMessage(err: unknown): string {
  if (err instanceof ConnectError) {
    return err.rawMessage || 'Request failed'
  }
  return err instanceof Error ? err.message : String(err)
}

export function isUnauthenticated(err: unknown): boolean {
  return err instanceof ConnectError && err.code === Code.Unauthenticated
}
