// ⌘K / "/" — jump to a server, a page or a setting from the keyboard.
import React, { useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { useApp } from '../lib/app-context'
import { fmtPct } from '../lib/format'
import { Icon } from './icons'
import { serverSubtitle, ServerState } from './Status'

export interface PaletteCommand { id: string; label: string; icon: string; hint?: string; keywords?: string; group?: string; run: () => void }

export function CommandPalette({ onClose, commands, openServer }: { onClose: () => void; commands: PaletteCommand[]; openServer: (id: number) => void }) {
  const { servers } = useApp()
  const [q, setQ] = useState('')
  const [hi, setHi] = useState(0)
  const list = useRef<HTMLDivElement>(null)

  type Row = { key: string; group: string; icon: React.ReactNode; label: React.ReactNode; sub?: React.ReactNode; hint?: string; run: () => void }
  const rows = useMemo<Row[]>(() => {
    const n = q.trim().toLowerCase()
    const out: Row[] = []
    for (const s of servers) {
      const hay = `${s.name} ${s.summary?.hostname || ''} ${s.summary?.platform || ''} ${s.url} ${s.kind}`.toLowerCase()
      if (n && !hay.includes(n)) continue
      out.push({
        key: 's' + s.id, group: 'Servers', icon: <ServerState status={s.status} />, label: s.name,
        sub: <>{serverSubtitle(s)}{s.status === 'up' && s.summary ? ` · CPU ${fmtPct(s.summary.cpu)} · RAM ${fmtPct(s.summary.mem_pct)}` : ''}</>, run: () => openServer(s.id),
      })
    }
    for (const c of commands) {
      if (n && !`${c.label} ${c.keywords || ''}`.toLowerCase().includes(n)) continue
      out.push({ key: c.id, group: c.group || 'Commands', icon: <Icon name={c.icon} size={16} />, label: c.label, hint: c.hint, run: c.run })
    }
    return out
  }, [q, servers, commands, openServer])
  useEffect(() => setHi(0), [q])
  useEffect(() => { list.current?.querySelector(`[data-i="${hi}"]`)?.scrollIntoView({ block: 'nearest' }) }, [hi])

  const run = (r: Row | undefined) => { if (!r) return; onClose(); r.run() }
  let last = ''
  return createPortal(
    <div className="palette-backdrop" onMouseDown={e => { if (e.target === e.currentTarget) onClose() }}>
      <div className="palette-box" role="dialog" aria-label="Search and commands">
        <div className="pal-input">
          <Icon name="search" size={18} />
          <input autoFocus value={q} onChange={e => setQ(e.target.value)} placeholder="Jump to a server, a page or a setting…" aria-label="Search"
            onKeyDown={e => {
              if (e.key === 'ArrowDown') { e.preventDefault(); setHi(h => Math.min(rows.length - 1, h + 1)) }
              else if (e.key === 'ArrowUp') { e.preventDefault(); setHi(h => Math.max(0, h - 1)) }
              else if (e.key === 'Enter') { e.preventDefault(); run(rows[hi]) }
              else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); onClose() }
            }} />
          <kbd>esc</kbd>
        </div>
        <div className="pal-list" ref={list} role="listbox">
          {rows.length === 0 && <div className="pal-empty">Nothing matches.</div>}
          {rows.map((r, i) => {
            const head = r.group !== last ? <div className="pal-group" key={'g' + r.group}>{r.group}</div> : null
            last = r.group
            return (
              <React.Fragment key={r.key}>
                {head}
                <div data-i={i} role="option" aria-selected={i === hi} className={`pal-row ${i === hi ? 'hi' : ''}`} onMouseEnter={() => setHi(i)} onClick={() => run(r)}>
                  <span className="pal-ico">{r.icon}</span>
                  <span className="grow"><span className="pal-label">{r.label}</span>{r.sub && <span className="pal-sub">{r.sub}</span>}</span>
                  {r.hint && <kbd>{r.hint}</kbd>}
                  {i === hi && <Icon name="chevron-right" size={14} className="muted" />}
                </div>
              </React.Fragment>
            )
          })}
        </div>
      </div>
    </div>, document.body)
}
