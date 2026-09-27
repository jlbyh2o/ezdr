import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'

import { errorMessage } from '@/lib/api'

// usePoll loads data immediately and then every intervalMs (if given).
// reload() fetches again on demand, for example after a change.
export function usePoll<T>(load: () => Promise<T>, intervalMs?: number) {
  const [data, setData] = useState<T>()
  const [error, setError] = useState<string>()
  const loadRef = useRef(load)
  useLayoutEffect(() => {
    loadRef.current = load
  })

  const reload = useCallback(async () => {
    try {
      setData(await loadRef.current())
      setError(undefined)
    } catch (err) {
      setError(errorMessage(err))
    }
  }, [])

  useEffect(() => {
    void reload()
    if (!intervalMs) return
    const id = setInterval(() => void reload(), intervalMs)
    return () => clearInterval(id)
  }, [reload, intervalMs])

  return { data, error, reload }
}
