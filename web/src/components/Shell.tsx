// The app frame shared by Overview, a server and Alerts: sidebar (a drawer
// on narrow screens), top bar, command palette, shortcuts.
import React, { useCallback, useEffect, useRef, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { useApp } from '../lib/app-context'
import { CommandPalette, type PaletteCommand } from './CommandPalette'
import { AddServerHost, openAddServer } from './ServerDialogs'
import { ShortcutsSheet } from './ShortcutsSheet'
import { Sidebar } from './Sidebar'
import { TopBar } from './TopBar'
import { modalOpen, useIsMobile, useMediaQuery } from './ui'

const SIDEBAR_KEY = 'pmon-sidebar'

const SETTINGS_CMDS: [string, string, string, string][] = [
  ['general', 'Settings: General', 'settings', 'theme units title range'],
  ['servers', 'Settings: Servers', 'server', 'add agent glances share'],
  ['rules', 'Settings: Alert rules', 'bell', 'threshold cpu memory disk temperature'],
  ['notifications', 'Settings: Notifications', 'mail', 'apprise ntfy webhook'],
  ['profile', 'Settings: Profile', 'user', 'password name'],
  ['tokens', 'Settings: Tokens', 'key', 'api'],
  ['about', 'Settings: About', 'info', 'version core'],
]

export function Shell({ title, sub, controls, loading, onKey, extraCommands = [], children }: {
  title: React.ReactNode
  sub?: React.ReactNode
  controls?: React.ReactNode
  loading?: boolean
  /** Page shortcuts; return true when the key was used. */
  onKey?: (k: string, e: KeyboardEvent) => boolean
  extraCommands?: PaletteCommand[]
  children: React.ReactNode
}) {
  const app = useApp()
  const navigate = useNavigate()
  const { id } = useParams()
  const mobile = useIsMobile()
  const narrow = useMediaQuery('(max-width: 1099px)')
  const drawer = mobile || narrow
  const [sidebarOpen, setSidebarOpen] = useState(() => !drawer && localStorage.getItem(SIDEBAR_KEY) !== '0')
  useEffect(() => { setSidebarOpen(!drawer && localStorage.getItem(SIDEBAR_KEY) !== '0') }, [drawer])
  const openedAt = useRef(0)
  const toggleSidebar = () => setSidebarOpen(o => { openedAt.current = performance.now(); const n = !o; if (!drawer) localStorage.setItem(SIDEBAR_KEY, n ? '1' : '0'); return n })
  const [palette, setPalette] = useState(false)
  const [shortcuts, setShortcuts] = useState(false)
  useEffect(() => {
    const h = () => setShortcuts(true)
    window.addEventListener('pmon:shortcuts', h)
    return () => window.removeEventListener('pmon:shortcuts', h)
  }, [])

  const openServer = useCallback((sid: number) => navigate(`/server/${sid}`), [navigate])
  const step = (dir: 1 | -1) => {
    const list = app.servers
    if (!list.length) return
    const i = list.findIndex(s => String(s.id) === id)
    const next = list[(i + dir + list.length) % list.length] || list[0]
    openServer(next.id)
  }

  // "g" then a letter, like most dashboards.
  const pendingG = useRef(0)
  useEffect(() => {
    const h = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement
      const typing = t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)
      const mod = e.metaKey || e.ctrlKey
      if (mod && e.key.toLowerCase() === 'k') { e.preventDefault(); if (!modalOpen()) setPalette(true); return }
      if (typing || modalOpen() || mod || e.altKey) return
      const k = e.key
      if (Date.now() - pendingG.current < 1200) {
        pendingG.current = 0
        if (k === 'o') { navigate('/'); return }
        if (k === 'a') { navigate('/alerts'); return }
        if (k === 's') { navigate('/settings'); return }
      }
      if (k === 'g') { pendingG.current = Date.now(); return }
      if (onKey?.(k, e)) return
      if (k === '/') { e.preventDefault(); setPalette(true) }
      else if (k === '?') setShortcuts(true)
      else if (k === 'n') { e.preventDefault(); openAddServer() }
      else if (k === 'j') step(1)
      else if (k === 'k') step(-1)
    }
    window.addEventListener('keydown', h)
    return () => window.removeEventListener('keydown', h)
  })

  const commands: PaletteCommand[] = [
    ...extraCommands,
    { id: 'add', label: 'Add a server', icon: 'plus', hint: 'N', keywords: 'new agent glances', run: openAddServer },
    { id: 'overview', label: 'Overview', icon: 'grid', keywords: 'home dashboard all servers', group: 'Go to', run: () => navigate('/') },
    { id: 'alerts', label: 'Alerts', icon: 'bell', keywords: 'warnings problems history', group: 'Go to', run: () => navigate('/alerts') },
    ...SETTINGS_CMDS.filter(([tab]) => tab !== 'people' || app.user?.role === 'admin').map(([tab, label, icon, kw]) => ({ id: 'set-' + tab, label, icon, keywords: kw, group: 'Settings', run: () => navigate(`/settings/${tab}`) })),
    ...(app.user?.role === 'admin' ? [{ id: 'set-people', label: 'Settings: People', icon: 'users', keywords: 'accounts users', group: 'Settings', run: () => navigate('/settings/people') }] : []),
    { id: 'theme', label: app.theme === 'dark' ? 'Switch to light mode' : 'Switch to dark mode', icon: app.theme === 'dark' ? 'sun' : 'moon', keywords: 'theme appearance', run: () => app.setThemeMode(app.theme === 'dark' ? 'light' : 'dark') },
    { id: 'shortcuts', label: 'Keyboard shortcuts', icon: 'keyboard', hint: '?', run: () => setShortcuts(true) },
  ]

  return (
    <div className={`app ${sidebarOpen ? 'with-sidebar' : 'no-sidebar'} ${mobile ? 'is-mobile' : ''} ${drawer ? 'drawer-mode' : ''}`}>
      {drawer && sidebarOpen && <div className="drawer-backdrop" onClick={e => { if (e.timeStamp - openedAt.current > 400) setSidebarOpen(false) }} />}
      <div className={`sidebar-wrap ${sidebarOpen ? 'open' : ''}`}>
        <Sidebar onNavigate={() => drawer && setSidebarOpen(false)} />
      </div>
      <main className="main">
        <TopBar title={title} sub={sub} controls={controls} onPalette={() => setPalette(true)} onMenu={toggleSidebar} mobile={mobile} compact={narrow} loading={loading} sidebarOpen={sidebarOpen} />
        <div className="page">{children}</div>
      </main>
      {palette && <CommandPalette onClose={() => setPalette(false)} commands={commands} openServer={openServer} />}
      {shortcuts && <ShortcutsSheet onClose={() => setShortcuts(false)} />}
      <AddServerHost />
    </div>
  )
}
