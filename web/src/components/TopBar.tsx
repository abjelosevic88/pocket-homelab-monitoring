import React from 'react'
import { useNavigate } from 'react-router-dom'
import { api, V1 } from '../lib/api'
import { useApp } from '../lib/app-context'
import { Icon } from './icons'
import { Avatar, MenuButton } from './ui'
import { AskButton } from './AskButton'

export function TopBar({ title, sub, controls, onPalette, onMenu, mobile, compact, loading, sidebarOpen }: {
  title: React.ReactNode
  sub?: React.ReactNode
  /** Filters that scope the whole page (the range control), shown on the right. */
  controls?: React.ReactNode
  onPalette: () => void
  onMenu: () => void
  mobile: boolean
  compact?: boolean
  loading?: boolean
  sidebarOpen: boolean
}) {
  const { user, themeMode, setThemeMode, status, refresh } = useApp()
  const navigate = useNavigate()
  const signOut = async () => { await api.post(`${V1}/auth/logout`).catch(() => {}); await refresh(); navigate('/login') }
  const avatarMenu = () => [
    { heading: user?.name || user?.email || '' },
    { label: 'Profile', icon: 'user', onSelect: () => navigate('/settings/profile') },
    { label: 'Settings', icon: 'settings', onSelect: () => navigate('/settings') },
    { label: 'Keyboard shortcuts', icon: 'keyboard', onSelect: () => window.dispatchEvent(new CustomEvent('pmon:shortcuts')), hint: '?' },
    'sep' as const,
    { heading: 'Appearance' },
    { label: 'System', checked: themeMode === 'system', onSelect: () => setThemeMode('system') },
    { label: 'Light', checked: themeMode === 'light', onSelect: () => setThemeMode('light') },
    { label: 'Dark', checked: themeMode === 'dark', onSelect: () => setThemeMode('dark') },
    ...(status?.auth_mode === 'local' ? ['sep' as const, { label: 'Sign out', icon: 'log-out', onSelect: signOut }] : []),
  ]
  return (
    <header className={`topbar ${loading ? 'is-loading' : ''}`}>
      <button type="button" className="btn ghost icon-only" onClick={onMenu} aria-label={sidebarOpen ? 'Hide the sidebar' : 'Show the sidebar'} title="Menu"><Icon name={mobile || compact ? 'menu' : 'sidebar'} size={18} /></button>
      <div className="tb-title">
        <h1 className="range-title" aria-live="polite">{title}</h1>
        {sub && !mobile && <span className="tb-sub ellipsis">{sub}</span>}
      </div>
      <div className="topbar-load" aria-hidden="true" />
      <span className="spacer" />
      {controls}
      <button type="button" className="btn ghost icon-only" onClick={onPalette} aria-label="Search" title="Search (⌘K or /)"><Icon name="search" size={18} /></button>
      {mobile && <AskButton />}
      <MenuButton label="Account" className="avatar-btn" align="right" width={220} items={avatarMenu}>
        <Avatar name={user?.name} email={user?.email} size={30} />
      </MenuButton>
    </header>
  )
}
