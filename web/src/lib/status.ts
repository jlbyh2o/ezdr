import { HealthState, PlanState } from '@/gen/ezdr/portal/v1/portal_pb'

// Status colors, shared by every page (docs/design/ui.md, section 2.2):
// success is healthy or running, warning needs attention, destructive is
// failed, info is in progress, and neutral is inactive or standby.
export type Tone = 'success' | 'warning' | 'destructive' | 'info' | 'neutral'

export const planStates: Record<PlanState, { label: string; tone: Tone }> = {
  [PlanState.UNSPECIFIED]: { label: 'Unknown', tone: 'neutral' },
  [PlanState.DRAFT]: { label: 'Draft', tone: 'neutral' },
  [PlanState.ACTIVE]: { label: 'Active', tone: 'success' },
  [PlanState.PAUSED]: { label: 'Paused', tone: 'neutral' },
  [PlanState.FAILING_OVER]: { label: 'Failing over', tone: 'info' },
  [PlanState.FAILED_OVER]: { label: 'Failed over', tone: 'warning' },
  [PlanState.FAILING_BACK]: { label: 'Failing back', tone: 'info' },
}

// Health of plans without one (drafts, paused plans) has no label.
export const healthStates: Record<HealthState, { label: string; tone: Tone }> = {
  [HealthState.UNSPECIFIED]: { label: '', tone: 'neutral' },
  [HealthState.NONE]: { label: '', tone: 'neutral' },
  [HealthState.HEALTHY]: { label: 'Healthy', tone: 'success' },
  [HealthState.SYNCING]: { label: 'Initial sync', tone: 'info' },
  [HealthState.LAGGING]: { label: 'Lagging', tone: 'warning' },
  [HealthState.FAILING]: { label: 'Failing', tone: 'destructive' },
  [HealthState.UNKNOWN]: { label: 'Unknown', tone: 'neutral' },
}
