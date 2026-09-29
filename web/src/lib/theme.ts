import { useEffect, useState } from 'react'

// The color theme: light, dark, or following the system, remembered in the
// browser.
export type Theme = 'light' | 'dark' | 'system'

const storageKey = 'ezdr-theme'
const dark = window.matchMedia('(prefers-color-scheme: dark)')

function stored(): Theme {
  const t = localStorage.getItem(storageKey)
  return t === 'light' || t === 'dark' ? t : 'system'
}

// applyTheme sets the dark class on the document for the stored theme.
export function applyTheme(theme: Theme = stored()) {
  const isDark = theme === 'dark' || (theme === 'system' && dark.matches)
  document.documentElement.classList.toggle('dark', isDark)
  document.documentElement.style.colorScheme = isDark ? 'dark' : 'light'
}

export function useTheme(): [Theme, (t: Theme) => void] {
  const [theme, setTheme] = useState<Theme>(stored)
  useEffect(() => {
    applyTheme(theme)
    if (theme !== 'system') return
    const follow = () => applyTheme('system')
    dark.addEventListener('change', follow)
    return () => dark.removeEventListener('change', follow)
  }, [theme])
  return [
    theme,
    (t: Theme) => {
      if (t === 'system') localStorage.removeItem(storageKey)
      else localStorage.setItem(storageKey, t)
      setTheme(t)
    },
  ]
}
