import React, { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import { api, V1 } from './api'
import type { Alert, AuthStatus, Server, Settings, SettingsResponse, User } from './types'

export type ThemeMode = 'system' | 'light' | 'dark'

interface AppState {
  status: AuthStatus | null
  user: User | null
  sr: SettingsResponse | null
  settings: Settings | null
  servers: Server[]
  serversLoaded: boolean
  alerts: Alert[]
  /** Bumps whenever something changed on the server: pages refetch. */
  version: number
  bump: () => void
  h12: boolean
  themeMode: ThemeMode
  theme: 'light' | 'dark'
  setThemeMode: (m: ThemeMode) => void
  refresh: () => Promise<void>
  reloadServers: () => Promise<void>
  reloadAlerts: () => Promise<void>
  saveSettings: (patch: Partial<Settings> & { notify_token?: string }) => Promise<SettingsResponse>
  setUser: (u: User | null) => void
  setServers: React.Dispatch<React.SetStateAction<Server[]>>
}

const Ctx = createContext<AppState>(null!)
export function useApp() { return useContext(Ctx) }

const THEME_KEY = 'pmon-theme'

export function AppProvider({ children }: { children: React.ReactNode }) {
  const [status, setStatus] = useState<AuthStatus | null>(null)
  const [user, setUser] = useState<User | null>(null)
  const [sr, setSr] = useState<SettingsResponse | null>(null)
  const [servers, setServers] = useState<Server[]>([])
  const [serversLoaded, setServersLoaded] = useState(false)
  const [alerts, setAlerts] = useState<Alert[]>([])
  const [version, setVersion] = useState(0)
  const [themeMode, setThemeModeRaw] = useState<ThemeMode>(() => (localStorage.getItem(THEME_KEY) as ThemeMode) || 'system')
  const [systemDark, setSystemDark] = useState(() => window.matchMedia('(prefers-color-scheme: dark)').matches)

  useEffect(() => {
    const mq = window.matchMedia('(prefers-color-scheme: dark)')
    const h = () => setSystemDark(mq.matches)
    mq.addEventListener('change', h)
    return () => mq.removeEventListener('change', h)
  }, [])
  const theme: 'light' | 'dark' = themeMode === 'system' ? (systemDark ? 'dark' : 'light') : themeMode
  useEffect(() => {
    document.documentElement.dataset.theme = theme
    document.querySelectorAll('meta[name="theme-color"]').forEach(m => m.setAttribute('content', theme === 'dark' ? '#121214' : '#ffffff'))
  }, [theme])
  const setThemeMode = useCallback((m: ThemeMode) => { localStorage.setItem(THEME_KEY, m); setThemeModeRaw(m) }, [])

  const settings = sr?.settings ?? null
  useEffect(() => { if (settings?.title) document.title = settings.title }, [settings?.title])
  useEffect(() => { document.documentElement.dataset.density = settings?.density || 'comfortable' }, [settings?.density])

  const reloadServers = useCallback(async () => {
    const list = await api.get<Server[]>(`${V1}/servers`)
    setServers(list || [])
    setServersLoaded(true)
  }, [])
  const reloadAlerts = useCallback(async () => {
    setAlerts(await api.get<Alert[]>(`${V1}/alerts?state=active`).then(a => a || []).catch(() => []))
  }, [])

  const refresh = useCallback(async () => {
    const st = await api.get<AuthStatus>(`${V1}/auth/status`)
    setStatus(st)
    setUser(st.user)
    if (st.authenticated) {
      const [s] = await Promise.all([
        api.get<SettingsResponse>(`${V1}/settings`),
        reloadServers().catch(() => setServersLoaded(true)),
        reloadAlerts(),
      ])
      setSr(s)
    }
  }, [reloadServers, reloadAlerts])

  useEffect(() => {
    refresh().catch(() => setStatus({ needs_setup: false, auth_mode: 'local', authenticated: false, user: null, version: '', title: 'Pocket Monitoring' }))
  }, [refresh])
  useEffect(() => {
    const h = () => { setUser(null); setStatus(s => s ? { ...s, authenticated: false, user: null } : s) }
    window.addEventListener('pmon:unauthorized', h)
    return () => window.removeEventListener('pmon:unauthorized', h)
  }, [])

  const bump = useCallback(() => setVersion(v => v + 1), [])

  const saveSettings = useCallback(async (patch: Partial<Settings> & { notify_token?: string }) => {
    setSr(s => s ? { ...s, settings: { ...s.settings, ...patch } } : s)
    try {
      const r = await api.put<SettingsResponse>(`${V1}/settings`, patch)
      setSr(r)
      return r
    } catch (e) {
      setSr(await api.get<SettingsResponse>(`${V1}/settings`))
      throw e
    }
  }, [])

  // The server list (statuses, headline numbers, sparklines) refreshes every
  // 5 s while the tab is visible; /changes catches adds, removes and alerts.
  const stamp = useRef<number | null>(null)
  useEffect(() => {
    if (!user) return
    const tick = async () => {
      if (document.hidden) return
      reloadServers().catch(() => {})
      try {
        const r = await api.get<{ stamp: number }>(`${V1}/changes`)
        if (stamp.current !== null && r.stamp !== stamp.current) { bump(); reloadAlerts() }
        stamp.current = r.stamp
      } catch { /* offline: try again later */ }
    }
    const t = setInterval(tick, 5000)
    const onShow = () => { if (!document.hidden) tick() }
    document.addEventListener('visibilitychange', onShow)
    window.addEventListener('focus', onShow)
    return () => { clearInterval(t); document.removeEventListener('visibilitychange', onShow); window.removeEventListener('focus', onShow) }
  }, [user, bump, reloadServers, reloadAlerts])

  const value = useMemo<AppState>(() => ({
    status, user, sr, settings, servers, serversLoaded, alerts, version, bump, h12: settings?.time_format === '12h',
    themeMode, theme, setThemeMode, refresh, reloadServers, reloadAlerts, saveSettings, setUser, setServers,
  }), [status, user, sr, settings, servers, serversLoaded, alerts, version, bump, themeMode, theme, setThemeMode, refresh, reloadServers, reloadAlerts, saveSettings])

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>
}
