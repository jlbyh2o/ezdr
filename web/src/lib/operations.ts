import { FailbackState, FailoverState, TakeoverState, TestState, TestVerdict } from '@/gen/ezdr/portal/v1/portal_pb'
import type { Tone } from '@/lib/status'

// Labels and colors for operations' states, shared by the operation pages
// and the plan's history.
type Look = { label: string; tone: Tone; active?: boolean }

export const failoverStates: Record<FailoverState, Look> = {
  [FailoverState.UNSPECIFIED]: { label: 'Unknown', tone: 'neutral' },
  [FailoverState.RUNNING]: { label: 'Running', tone: 'info', active: true },
  [FailoverState.AWAITING_CONFIRMATION]: { label: 'Awaiting confirmation', tone: 'warning', active: true },
  [FailoverState.COMPLETED]: { label: 'Completed', tone: 'success' },
  [FailoverState.ABORTED]: { label: 'Undone', tone: 'neutral' },
  [FailoverState.FAILED]: { label: 'A step failed', tone: 'destructive', active: true },
}

export const failbackStates: Record<FailbackState, Look> = {
  [FailbackState.UNSPECIFIED]: { label: 'Unknown', tone: 'neutral' },
  [FailbackState.RUNNING]: { label: 'Running', tone: 'info', active: true },
  [FailbackState.AWAITING_CONFIRMATION]: { label: 'Awaiting confirmation', tone: 'warning', active: true },
  [FailbackState.COMPLETED]: { label: 'Completed', tone: 'success' },
  [FailbackState.ABORTED]: { label: 'Undone', tone: 'neutral' },
  [FailbackState.FAILED]: { label: 'A step failed', tone: 'destructive', active: true },
}

export const testStates: Record<TestState, Look> = {
  [TestState.UNSPECIFIED]: { label: 'Unknown', tone: 'neutral' },
  [TestState.STARTING]: { label: 'Starting', tone: 'info', active: true },
  [TestState.RUNNING]: { label: 'Running', tone: 'info', active: true },
  [TestState.ENDING]: { label: 'Ending', tone: 'info', active: true },
  [TestState.ENDED]: { label: 'Ended', tone: 'neutral' },
  [TestState.FAILED]: { label: 'Setup failed', tone: 'destructive' },
}

export const takeoverStates: Record<TakeoverState, Look> = {
  [TakeoverState.UNSPECIFIED]: { label: 'Unknown', tone: 'neutral' },
  [TakeoverState.READY]: { label: 'Ready', tone: 'neutral' },
  [TakeoverState.BLOCKED]: { label: 'Blocked', tone: 'warning' },
  [TakeoverState.RUNNING]: { label: 'Running', tone: 'info', active: true },
  [TakeoverState.COMPLETED]: { label: 'Completed', tone: 'success' },
  [TakeoverState.ROLLED_BACK]: { label: 'Rolled back', tone: 'warning' },
  [TakeoverState.FAILED]: { label: 'Failed', tone: 'destructive', active: true },
}

export const verdictLabel: Record<TestVerdict, string> = {
  [TestVerdict.UNSPECIFIED]: 'no verdict',
  [TestVerdict.PASSED]: 'passed',
  [TestVerdict.FAILED]: 'failed',
}

// Paths of operation pages. "latest" stands for the plan's newest failover
// or failback.
export const operationPath = {
  test: (planId: string, id: string) => `/plans/${planId}/tests/${id}`,
  failover: (planId: string, id = 'latest') => `/plans/${planId}/failovers/${id}`,
  failback: (planId: string, id = 'latest') => `/plans/${planId}/failbacks/${id}`,
  takeover: (planId: string) => `/plans/${planId}/takeover`,
}
