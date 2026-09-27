import { CircleAlert } from 'lucide-react'

import { Alert, AlertDescription } from '@/components/ui/alert'

export function ErrorAlert({ message }: { message?: string }) {
  if (!message) return null
  return (
    <Alert variant="destructive">
      <CircleAlert />
      <AlertDescription>{message}</AlertDescription>
    </Alert>
  )
}
