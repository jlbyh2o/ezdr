// Segmented is a row of mutually exclusive filters, such as All / Firing /
// Resolved, with optional counts.
export function Segmented<T extends string>({
  value,
  onChange,
  options,
  label,
}: {
  value: T
  onChange: (v: T) => void
  options: { value: T; label: string; count?: number }[]
  label: string
}) {
  return (
    <div role="radiogroup" aria-label={label} className="inline-flex w-fit flex-wrap gap-1 justify-self-start rounded-lg border bg-muted/40 p-1">
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          role="radio"
          aria-checked={value === o.value}
          onClick={() => onChange(o.value)}
          className={`flex items-center gap-1.5 rounded-md px-2.5 py-1 text-sm transition-colors ${
            value === o.value ? 'bg-card font-medium text-foreground shadow-xs' : 'text-muted-foreground hover:text-foreground'
          }`}
        >
          {o.label}
          {o.count !== undefined && <span className="text-xs text-muted-foreground tabular-nums">{o.count}</span>}
        </button>
      ))}
    </div>
  )
}
