// The EZDR mark: two arrows replicating between sites, on the accent color.
export function LogoMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 32 32" className={className} aria-hidden>
      <rect width="32" height="32" rx="7" className="fill-primary" />
      <path
        d="M9 12.5h12.5m0 0-3.5-3.5m3.5 3.5L18 16M23 19.5H10.5m0 0 3.5-3.5m-3.5 3.5L14 23"
        fill="none"
        stroke="white"
        strokeWidth="2.4"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  )
}
