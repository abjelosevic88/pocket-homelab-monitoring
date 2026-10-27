// Left column: add a server, Overview / Alerts, the server list.
import { useState } from 'react'
import { Link, NavLink } from 'react-router-dom'
import { useApp } from '../lib/app-context'
import { fmtPct } from '../lib/format'
import type { Server } from '../lib/types'
import { Icon } from './icons'
import { openAddServer, useServerActions } from './ServerDialogs'
import { ServerState } from './Status'
import { type MenuItem, MenuButton } from './ui'

export function Sidebar({ onNavigate }: { onNavigate?: () => void }) {
  const { servers, alerts, status } = useApp()
  const [open, setOpen] = useState(true)
  const actions = useServerActions()
  const mine = servers.filter(s => s.role === 'owner')
  const shared = servers.filter(s => s.role !== 'owner')
  const crit = alerts.some(a => a.severity === 'critical')
  return (
    <aside className="sidebar" aria-label="Servers">
      <div className="sb-brand">
        <img src="/favicon.svg" alt="" className="sb-logo" />
        <span className="sb-title">{status?.title && status.title !== 'Pocket Monitoring' ? status.title : <>Pocket <b>Monitoring</b></>}</span>
        {status?.version && <span className="sb-version" title={`Version ${status.version}`}>{/^v?\d+\.\d+/.test(status.version) ? 'v' + status.version.replace(/^v/, '').replace(/[-+].*$/, '') : status.version}</span>}
      </div>
      <button type="button" className="btn primary create-btn" onClick={() => { onNavigate?.(); openAddServer() }}><Icon name="plus" size={17} strokeWidth={2.2} />Add server<kbd className="kbd-on-primary">N</kbd></button>

      <nav className="sb-nav" aria-label="Pages">
        <NavLink to="/" end className="sb-nav-item" onClick={onNavigate}><Icon name="grid" size={16} />Overview</NavLink>
        <NavLink to="/alerts" className="sb-nav-item" onClick={onNavigate}><Icon name="bell" size={16} />Alerts
          {alerts.length > 0 && <span className={`badge ${crit ? 'danger' : 'warn'}`} title={`${alerts.length} active`}>{alerts.length}</span>}</NavLink>
      </nav>

      <section className="sb-sec">
        <h4 className="sb-h">
          <button type="button" className="sb-h-btn" aria-expanded={open} onClick={() => setOpen(o => !o)}><Icon name={open ? 'chevron-down' : 'chevron-right'} size={13} />Servers</button>
          <button type="button" className="btn ghost icon-only xs" aria-label="Add server" title="Add server" onClick={() => { onNavigate?.(); openAddServer() }}><Icon name="plus" size={14} /></button>
        </h4>
        {open && (
          <ul className="srv-list">
            {mine.map(s => <ServerRow key={s.id} s={s} items={actions.items(s)} onNavigate={onNavigate} />)}
            {shared.length > 0 && <li className="srv-sub">Shared with me</li>}
            {shared.map(s => <ServerRow key={s.id} s={s} items={actions.items(s)} onNavigate={onNavigate} />)}
            {!servers.length && <li className="srv-none muted small">No servers yet.</li>}
          </ul>
        )}
      </section>

      <div className="sb-foot">
        <Link to="/settings" className="sb-foot-link" onClick={onNavigate}><Icon name="settings" size={15} />Settings</Link>
        <button type="button" className="sb-foot-link" onClick={() => window.dispatchEvent(new CustomEvent('pmon:shortcuts'))}><Icon name="keyboard" size={15} />Shortcuts</button>
      </div>
      {actions.dialogs}
    </aside>
  )
}

function ServerRow({ s, items, onNavigate }: { s: Server; items: MenuItem[]; onNavigate?: () => void }) {
  const cpu = s.status === 'up' && s.summary ? s.summary.cpu : null
  return (
    <li className={`srv-row ${s.status}`} style={{ ['--c' as string]: s.color }}>
      <NavLink to={`/server/${s.id}`} className="srv-link" onClick={onNavigate} title={s.last_error || undefined}>
        <span className="srv-swatch" />
        <span className="srv-name ellipsis">{s.name}</span>
        {s.role !== 'owner' && <span className="cal-owner ellipsis">{(s.owner?.name || s.owner?.email || '').split(' ')[0]}</span>}
        {s.alerts_active > 0 && <span className="srv-alerts" title={`${s.alerts_active} active alert${s.alerts_active === 1 ? '' : 's'}`}><Icon name="bell" size={11} strokeWidth={2.2} />{s.alerts_active}</span>}
        {cpu !== null ? <span className="srv-cpu tnum" title="CPU now">{fmtPct(cpu)}</span> : null}
        <ServerState status={s.status} />
      </NavLink>
      <MenuButton label={`Options for ${s.name}`} className="btn ghost icon-only xs cal-more" items={items} width={210}><Icon name="more" size={15} /></MenuButton>
    </li>
  )
}
