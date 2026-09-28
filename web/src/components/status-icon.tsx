import { CheckCircle2, Circle, Loader2, MinusCircle, XCircle } from 'lucide-react'

const icons: Record<string, React.ReactNode> = {
  pending: <Circle className="size-4 text-muted-foreground" />,
  running: <Loader2 className="size-4 animate-spin" />,
  starting: <Loader2 className="size-4 animate-spin" />,
  done: <CheckCircle2 className="size-4 text-emerald-700" />,
  switched: <CheckCircle2 className="size-4 text-emerald-700" />,
  failed: <XCircle className="size-4 text-destructive" />,
  skipped: <MinusCircle className="size-4 text-muted-foreground" />,
}

// StatusIcon shows a step's or guest's status, such as "running" or "done".
export function StatusIcon({ status }: { status: string }) {
  return icons[status] ?? icons.pending
}
