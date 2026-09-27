import { useEffect, useState } from 'react'

type Health = { status: string; version: string }

function App() {
  const [health, setHealth] = useState<Health | null>(null)
  const [error, setError] = useState(false)

  useEffect(() => {
    fetch('/api/health')
      .then((res) => (res.ok ? res.json() : Promise.reject(res.status)))
      .then(setHealth)
      .catch(() => setError(true))
  }, [])

  return (
    <main className="mx-auto max-w-2xl p-8">
      <h1 className="text-3xl font-semibold">EZDR</h1>
      <p className="mt-2 text-gray-600">Easy Disaster Recovery for Proxmox VE</p>
      <p className="mt-6 text-sm text-gray-500">
        {error && 'Portal API is unreachable.'}
        {health && `Portal ${health.version}: ${health.status}`}
        {!error && !health && 'Checking portal status…'}
      </p>
    </main>
  )
}

export default App
