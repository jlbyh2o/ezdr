import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'

// RPO shows the abbreviation and explains it on hover.
export function RPO() {
  return (
    <Tooltip>
      <TooltipTrigger
        render={<abbr className="cursor-help underline decoration-dotted underline-offset-2">RPO</abbr>}
      />
      <TooltipContent className="max-w-72">
        Recovery Point Objective: how much recent data a failover would lose right now. It's the age of the oldest
        disk's newest copy on the DR host.
      </TooltipContent>
    </Tooltip>
  )
}
