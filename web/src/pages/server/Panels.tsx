// Panels drawn from the latest snapshot.
import React, { useState } from 'react'
import { useApp } from '../../lib/app-context'
import { fmtBytes, fmtDateTime, fmtDuration, fmtNum, fmtPct, fmtRate, fmtTemp, level, tempLevel } from '../../lib/format'
import type { Server, Snapshot } from '../../lib/types'
import { LevelBadge, Meter } from '../../components/Chart'
import { Icon } from '../../components/icons'
import { KIND_LABEL } from '../../components/ServerDialogs'
import { Empty, Switch } from '../../components/ui'
import { SortTh, useSort } from './sort'

export function Panel({ title, icon, actions, children, className = '', flush }: { title: string; icon: string; actions?: React.ReactNode; children: React.ReactNode; className?: string; flush?: boolean }) {
  return (
    <section className={`panel ${className}`}>
      <header className="panel-head"><Icon name={icon} size={15} className="muted" /><h3>{title}</h3><span className="spacer" />{actions}</header>
      <div className={`panel-body ${flush ? 'flush' : ''}`}>{children}</div>
    </section>
  )
}

// ---------- quicklook tiles ----------

function Tile({ label, icon, value, sub, pct, lvl }: { label: string; icon: string; value: React.ReactNode; sub?: React.ReactNode; pct?: number; lvl?: ReturnType<typeof level> }) {
  return (
    <div className="tile">
      <div className="tile-label"><Icon name={icon} size={14} />{label}</div>
      <div className="tile-value tnum">{value}</div>
      {pct !== undefined && <Meter value={pct} lvl={lvl || level(pct)} label={label} />}
      {sub && <div className="tile-sub muted small">{sub}</div>}
    </div>
  )
}

export function QuickTiles({ snap }: { snap: Snapshot }) {
  const { settings } = useApp()
  const unit = settings?.temp_unit || 'C'
  const hot = snap.sensors.filter(s => s.kind === 'temp' && s.value > 0 && s.value < 150).sort((a, b) => b.value - a.value)[0]
  const threads = snap.host.threads || snap.cpu.per_core.length || 1
  const perThread = snap.load.one / threads
  return (
    <div className="tiles">
      <Tile label="CPU" icon="cpu" value={fmtPct(snap.cpu.total)} pct={snap.cpu.total} sub={`${threads} threads${snap.cpu.freq_mhz ? ` · ${(snap.cpu.freq_mhz / 1000).toFixed(2)} GHz` : ''}`} />
      <Tile label="Memory" icon="memory" value={fmtPct(snap.mem.percent)} pct={snap.mem.percent} sub={`${fmtBytes(snap.mem.used)} of ${fmtBytes(snap.mem.total)}`} />
      <Tile label="Swap" icon="memory" value={snap.swap.total ? fmtPct(snap.swap.percent) : 'None'} pct={snap.swap.total ? snap.swap.percent : undefined} lvl={level(snap.swap.percent, 50, 80)}
        sub={snap.swap.total ? `${fmtBytes(snap.swap.used)} of ${fmtBytes(snap.swap.total)}` : 'No swap configured'} />
      <Tile label="Load" icon="gauge" value={snap.load.one.toFixed(2)} pct={Math.min(100, perThread * 100)} lvl={level(perThread * 100, 80, 150)}
        sub={<span title={`${perThread.toFixed(2)} per thread`}>{snap.load.five.toFixed(2)} · {snap.load.fifteen.toFixed(2)} · <span className="faint">{perThread.toFixed(2)}/thr</span></span>} />
      <Tile label="Hottest" icon="thermometer" value={hot ? fmtTemp(hot.value, unit) : '–'} pct={hot ? Math.min(100, (hot.value / (hot.critical || 100)) * 100) : undefined}
        lvl={hot ? tempLevel(hot.value, hot.high, hot.critical) : 'ok'} sub={hot ? hot.label : 'No temperature sensors'} />
      <Tile label="Uptime" icon="clock" value={fmtDuration(snap.host.uptime_s)} sub={snap.host.boot_time ? `since ${fmtDateTime(snap.host.boot_time)}` : undefined} />
      <Tile label="Processes" icon="list" value={fmtNum(snap.procs.total)} sub={<span title={`${fmtNum(snap.procs.threads)} threads`}>{snap.procs.running} running{snap.procs.zombie ? ` · ${snap.procs.zombie} zombie` : ''}</span>} />
    </div>
  )
}

// ---------- stacked bar (2px gaps, legend with numbers) ----------

function Stack({ parts, total, format, label }: { parts: { key: string; label: string; value: number }[]; total: number; format: (v: number) => string; label: string }) {
  const shown = parts.filter(p => p.value > 0)
  return (
    <div className="stack-bar-wrap">
      <div className="stack-bar" role="img" aria-label={`${label}: ${shown.map(p => `${p.label} ${format(p.value)}`).join(', ')}`}>
        {shown.map(p => <span key={p.key} className={`stack-seg s-${p.key}`} style={{ flexGrow: p.value / (total || 1) }} title={`${p.label}: ${format(p.value)}`} />)}
      </div>
      <ul className="stack-legend">
        {parts.map(p => <li key={p.key}><span className={`viz-sw s-${p.key}`} /><span className="muted">{p.label}</span><b className="tnum">{format(p.value)}</b></li>)}
      </ul>
    </div>
  )
}

export function CpuPanel({ snap }: { snap: Snapshot }) {
  const c = snap.cpu
  const other = Math.max(0, c.nice + c.irq + c.softirq)
  return (
    <Panel title="CPU" icon="cpu">
      <Stack label="CPU time" total={100} format={v => fmtPct(v, 1)} parts={[
        { key: 'c1', label: 'User', value: c.user }, { key: 'c2', label: 'System', value: c.system },
        { key: 'c3', label: 'I/O wait', value: c.iowait }, { key: 'c4', label: c.steal > 0.05 ? 'Steal + other' : 'Other', value: other + c.steal },
        { key: 'free', label: 'Idle', value: Math.max(0, 100 - c.user - c.system - c.iowait - other - c.steal) },
      ]} />
      {c.per_core.length > 0 && (
        <div className="cores" role="list" aria-label="Per thread">
          {c.per_core.map((v, i) => (
            <div key={i} className={`core ${level(v)}`} role="listitem" title={`Thread ${i}: ${fmtPct(v)}`}>
              <span className="core-bar"><span style={{ height: `${Math.max(2, v)}%` }} /></span>
              <span className="core-n tnum">{i}</span>
            </div>
          ))}
        </div>
      )}
      <dl className="kv">
        {snap.host.cpu_model && <div><dt>Model</dt><dd>{snap.host.cpu_model}</dd></div>}
        {c.freq_mhz > 0 && <div><dt>Frequency</dt><dd className="tnum">{(c.freq_mhz / 1000).toFixed(2)} GHz</dd></div>}
        <div><dt>Context switches</dt><dd className="tnum">{fmtNum(c.ctx_switches_s)}/s</dd></div>
        <div><dt>Interrupts</dt><dd className="tnum">{fmtNum(c.interrupts_s)}/s</dd></div>
      </dl>
    </Panel>
  )
}

export function MemPanel({ snap }: { snap: Snapshot }) {
  const m = snap.mem
  const appUsed = Math.max(0, m.total - m.free - m.buffers - m.cached)
  return (
    <Panel title="Memory" icon="memory">
      <Stack label="Memory" total={m.total} format={v => fmtBytes(v)} parts={[
        { key: 'c1', label: 'Used by apps', value: appUsed }, { key: 'c2', label: 'Buffers', value: m.buffers },
        { key: 'c3', label: 'Cache', value: m.cached }, { key: 'free', label: 'Free', value: m.free },
      ]} />
      <dl className="kv">
        <div><dt>Total</dt><dd className="tnum">{fmtBytes(m.total)}</dd></div>
        <div><dt>Available</dt><dd className="tnum">{fmtBytes(m.available)} <span className="muted">({fmtPct(100 - m.percent)})</span></dd></div>
        {m.shared > 0 && <div><dt>Shared</dt><dd className="tnum">{fmtBytes(m.shared)}</dd></div>}
        <div><dt>Swap</dt><dd className="tnum">{snap.swap.total ? <>{fmtBytes(snap.swap.used)} of {fmtBytes(snap.swap.total)}{(snap.swap.in_bps > 0 || snap.swap.out_bps > 0) && <span className="muted"> · in {fmtRate(snap.swap.in_bps)} · out {fmtRate(snap.swap.out_bps)}</span>}</> : 'none'}</dd></div>
      </dl>
    </Panel>
  )
}

export function SensorsPanel({ snap }: { snap: Snapshot }) {
  const { settings } = useApp()
  const unit = settings?.temp_unit || 'C'
  const temps = snap.sensors.filter(s => s.kind === 'temp')
  const other = snap.sensors.filter(s => s.kind !== 'temp')
  if (!snap.sensors.length) return <Panel title="Sensors" icon="thermometer"><p className="muted small">No sensors were found on this machine (common in VMs).</p></Panel>
  return (
    <Panel title="Sensors" icon="thermometer">
      <ul className="sensor-list">
        {temps.map(s => {
          const lvl = tempLevel(s.value, s.high, s.critical)
          const scale = s.critical || 100
          return (
            <li key={s.key}>
              <span className="sensor-name ellipsis" title={`${s.label} (${s.chip})`}>{s.label}<span className="faint xsmall"> {s.chip}</span></span>
              <Meter value={(s.value / scale) * 100} lvl={lvl} label={s.label} thin showLevel={false} />
              <span className="sensor-val tnum">{fmtTemp(s.value, unit)}</span>
              {lvl !== 'ok' ? <LevelBadge lvl={lvl} /> : <span className="faint xsmall tnum sensor-lim">{s.critical ? `crit ${fmtTemp(s.critical, unit)}` : s.high ? `high ${fmtTemp(s.high, unit)}` : ''}</span>}
            </li>
          )
        })}
      </ul>
      {other.length > 0 && (
        <dl className="kv mt-s">
          {other.map(s => <div key={s.key}><dt><Icon name={s.kind === 'fan' ? 'fan' : 'zap'} size={13} />{s.label}</dt><dd className="tnum">{fmtNum(s.value, s.kind === 'fan' ? 0 : 1)} {s.unit}</dd></div>)}
        </dl>
      )}
    </Panel>
  )
}

export function FsPanel({ snap }: { snap: Snapshot }) {
  const sort = useSort(snap.fs, 'mount', 'asc')
  return (
    <Panel title="File systems" icon="disk" flush>
      {!snap.fs.length ? <p className="muted small pad">No file systems reported.</p> : (
        <div className="tbl-wrap">
          <table className="table dense">
            <thead><tr>
              <SortTh sort={sort} k="mount" label="Mount" />
              <th className="hide-sm">Device</th>
              <SortTh sort={sort} k="percent" label="Used" className="w-meter" />
              <SortTh sort={sort} k="free" label="Free" right />
              <SortTh sort={sort} k="total" label="Size" right className="hide-sm" />
              <th className="right hide-sm">Inodes</th>
            </tr></thead>
            <tbody>
              {sort.rows.map(f => {
                const lvl = level(f.percent, 80, 90)
                return (
                  <tr key={f.mount}>
                    <td><span className="mono">{f.mount}</span>{f.network && <span className="tag">network</span>}{f.read_only && <span className="tag">read-only</span>}</td>
                    <td className="hide-sm muted small ellipsis-cell" title={`${f.device} (${f.type})`}>{f.device} <span className="faint">{f.type}</span></td>
                    <td className="w-meter"><div className="cell-meter"><Meter value={f.percent} lvl={lvl} label={`${f.mount} used`} thin showLevel={false} /><span className="tnum">{fmtPct(f.percent)}</span>{lvl !== 'ok' && <Icon name="alert" size={12} className={`lvl-ico ${lvl}`} title={lvl === 'crit' ? 'Critical' : 'High'} />}</div></td>
                    <td className="right tnum">{fmtBytes(f.free)}</td>
                    <td className="right tnum hide-sm">{fmtBytes(f.total)}</td>
                    <td className="right tnum hide-sm muted">{f.inodes_percent ? fmtPct(f.inodes_percent) : '–'}</td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}
    </Panel>
  )
}

export function NetPanel({ snap }: { snap: Snapshot }) {
  const [all, setAll] = useState(false)
  const rows = snap.net.filter(n => all || !n.virtual)
  const sort = useSort(rows, 'rx_bps', 'desc')
  const hidden = snap.net.length - snap.net.filter(n => !n.virtual).length
  return (
    <Panel title="Network interfaces" icon="network" flush actions={hidden > 0 && (
      <label className="inline-switch small"><Switch size="sm" checked={all} onChange={setAll} label="Show virtual interfaces" />Virtual ({hidden})</label>
    )}>
      {!rows.length ? <p className="muted small pad">No interfaces.</p> : (
        <div className="tbl-wrap">
          <table className="table dense">
            <thead><tr>
              <SortTh sort={sort} k="name" label="Interface" />
              <SortTh sort={sort} k="rx_bps" label="Receive" right />
              <SortTh sort={sort} k="tx_bps" label="Send" right />
              <SortTh sort={sort} k="rx_total" label="Total in" right className="hide-sm" />
              <SortTh sort={sort} k="tx_total" label="Total out" right className="hide-sm" />
              <th className="hide-sm">Link</th>
              <th className="hide-md">Addresses</th>
            </tr></thead>
            <tbody>
              {sort.rows.map(n => (
                <tr key={n.name} className={n.up ? '' : 'dim'}>
                  <td><span className="mono">{n.name}</span>{!n.up && <span className="tag">down</span>}{n.virtual && <span className="tag">virtual</span>}</td>
                  <td className="right tnum">{fmtRate(n.rx_bps)}</td>
                  <td className="right tnum">{fmtRate(n.tx_bps)}</td>
                  <td className="right tnum hide-sm muted">{fmtBytes(n.rx_total)}</td>
                  <td className="right tnum hide-sm muted">{fmtBytes(n.tx_total)}</td>
                  <td className="hide-sm muted small tnum">{n.speed_mbps ? (n.speed_mbps >= 1000 ? `${n.speed_mbps / 1000} Gb/s` : `${n.speed_mbps} Mb/s`) : '–'}{n.rx_errors + n.tx_errors > 0 && <span className="tag warn">{n.rx_errors + n.tx_errors} errors</span>}</td>
                  <td className="hide-md small mono ellipsis-cell" title={(n.addrs || []).join(', ')}>{(n.addrs || []).join(', ') || <span className="faint">–</span>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Panel>
  )
}

export function DiskPanel({ snap }: { snap: Snapshot }) {
  const sort = useSort(snap.disk_io, 'name', 'asc')
  return (
    <Panel title="Disk I/O" icon="disk" flush>
      {!snap.disk_io.length ? <p className="muted small pad">No block devices reported.</p> : (
        <div className="tbl-wrap">
          <table className="table dense">
            <thead><tr>
              <SortTh sort={sort} k="name" label="Device" />
              <SortTh sort={sort} k="read_bps" label="Read" right />
              <SortTh sort={sort} k="write_bps" label="Write" right />
              <SortTh sort={sort} k="read_iops" label="Read IOPS" right className="hide-sm" />
              <SortTh sort={sort} k="write_iops" label="Write IOPS" right className="hide-sm" />
              <SortTh sort={sort} k="busy" label="Busy" className="w-meter" />
            </tr></thead>
            <tbody>
              {sort.rows.map(d => (
                <tr key={d.name}>
                  <td className="mono">{d.name}</td>
                  <td className="right tnum">{fmtRate(d.read_bps)}</td>
                  <td className="right tnum">{fmtRate(d.write_bps)}</td>
                  <td className="right tnum hide-sm muted">{fmtNum(d.read_iops)}</td>
                  <td className="right tnum hide-sm muted">{fmtNum(d.write_iops)}</td>
                  <td className="w-meter"><div className="cell-meter"><Meter value={d.busy} lvl={level(d.busy, 80, 95)} label={`${d.name} busy`} thin showLevel={false} /><span className="tnum">{fmtPct(d.busy)}</span></div></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Panel>
  )
}

export function RaidPanel({ snap }: { snap: Snapshot }) {
  if (!snap.raid?.length) return null
  return (
    <Panel title="Software RAID" icon="shield">
      <ul className="raid-list">
        {snap.raid.map(r => (
          <li key={r.name}>
            <span className="mono">{r.name}</span><span className="muted small">{r.level}</span>
            <span className="tnum small">{r.active}/{r.devices} devices</span>
            <span className="spacer" />
            {r.sync ? <span className="small tnum">syncing {fmtPct(r.sync, 1)}</span> : null}
            {r.degraded ? <LevelBadge lvl="crit">Degraded</LevelBadge> : r.state !== 'active' ? <LevelBadge lvl="warn">{r.state}</LevelBadge> : <span className="lvl-badge ok"><Icon name="check-circle" size={12} />Healthy</span>}
          </li>
        ))}
      </ul>
    </Panel>
  )
}

export function SystemPanel({ snap, server }: { snap: Snapshot; server: Server }) {
  const h = snap.host
  const rows: [string, React.ReactNode][] = [
    ['Host name', h.hostname],
    ['System', h.platform || h.os],
    ['Kernel', h.kernel],
    ['Architecture', h.arch],
    ...(h.virtualization ? [['Virtualization', h.virtualization] as [string, React.ReactNode]] : []),
    ['Processor', h.cpu_model || '–'],
    ['Cores', `${h.cores || '?'} cores · ${h.threads || '?'} threads`],
    ['Memory', fmtBytes(snap.mem.total)],
    ['Booted', h.boot_time ? `${fmtDateTime(h.boot_time)} (${fmtDuration(h.uptime_s)} ago)` : '–'],
    ['Addresses', (h.ips || []).length ? <span className="mono small">{h.ips.join(', ')}</span> : '–'],
    ['Read through', `${KIND_LABEL[server.kind]}${h.agent_version ? ` ${h.agent_version}` : ''}${server.kind !== 'local' ? ` · ${server.url}` : ''}`],
    ['Every', `${server.interval_s} seconds`],
  ]
  return (
    <Panel title="System" icon="server">
      <dl className="set-dl">{rows.map(([k, v]) => <div key={k}><dt>{k}</dt><dd>{v}</dd></div>)}</dl>
    </Panel>
  )
}

export function NoData({ server }: { server: Server }) {
  return (
    <Empty title={server.status === 'paused' ? 'Paused' : server.last_error ? 'Can’t reach this server' : 'Waiting for the first reading'} icon={server.last_error ? 'alert' : 'clock'}>
      {server.status === 'paused' ? 'Resume it from the server menu to start reading again.' : server.last_error || 'Readings arrive every few seconds.'}
    </Empty>
  )
}
