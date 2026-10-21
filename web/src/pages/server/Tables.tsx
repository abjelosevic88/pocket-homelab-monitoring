// Processes and containers: searchable, sortable tables.
import { useState } from 'react'
import { fmtBytes, fmtDuration, fmtNum, fmtPct, fmtRate, level } from '../../lib/format'
import type { Container, Proc, Snapshot } from '../../lib/types'
import { LevelBadge } from '../../components/Chart'
import { Icon } from '../../components/icons'
import { Empty, Segmented } from '../../components/ui'
import { Panel } from './Panels'
import { SortTh, useSort } from './sort'

const STATE_LABEL: Record<string, string> = { R: 'running', S: 'sleeping', D: 'disk wait', Z: 'zombie', T: 'stopped', I: 'idle', t: 'traced', X: 'dead' }

export function Search({ value, onChange, placeholder }: { value: string; onChange: (v: string) => void; placeholder: string }) {
  return (
    <div className="tbl-search">
      <Icon name="search" size={14} />
      <input value={value} onChange={e => onChange(e.target.value)} placeholder={placeholder} aria-label={placeholder} />
      {value && <button type="button" className="btn ghost icon-only xs" aria-label="Clear" onClick={() => onChange('')}><Icon name="x" size={13} /></button>}
    </div>
  )
}

export function ProcessTable({ snap }: { snap: Snapshot }) {
  const [q, setQ] = useState('')
  const n = q.trim().toLowerCase()
  const rows = snap.procs.top.filter(p => !n || `${p.name} ${p.cmd} ${p.user} ${p.pid} ${p.container || ''}`.toLowerCase().includes(n))
  const sort = useSort<Proc>(rows, 'cpu', 'desc')
  const p = snap.procs
  return (
    <Panel title="Processes" icon="list" flush actions={<span className="muted small tnum">{fmtNum(p.total)} total · {p.running} running · {p.sleeping} sleeping{p.zombie ? ` · ${p.zombie} zombie` : ''} · {fmtNum(p.threads)} threads</span>}>
      <div className="tbl-tools">
        <Search value={q} onChange={setQ} placeholder="Filter by name, command, user, PID or container" />
        <span className="muted xsmall">The {snap.procs.top.length} busiest processes; CPU is per thread (200% = two busy threads).</span>
      </div>
      {!sort.rows.length ? <Empty title={n ? 'No process matches' : 'No processes reported'} icon="list" /> : (
        <div className="tbl-wrap">
          <table className="table dense proc-table">
            <thead><tr>
              <SortTh sort={sort} k="pid" label="PID" right />
              <SortTh sort={sort} k="name" label="Name" />
              <SortTh sort={sort} k="user" label="User" className="hide-sm" />
              <SortTh sort={sort} k="cpu" label="CPU" right />
              <SortTh sort={sort} k="mem_pct" label="Mem" right />
              <SortTh sort={sort} k="rss" label="RSS" right className="hide-sm" />
              <SortTh sort={sort} k="threads" label="Thr" right className="hide-sm" />
              <th className="hide-md">State</th>
              <SortTh sort={sort} k="container" label="Container" className="hide-md" />
              <th className="hide-sm">Command</th>
            </tr></thead>
            <tbody>
              {sort.rows.map(pr => {
                const lvl = level(pr.cpu, 80, 100)
                return (
                  <tr key={pr.pid}>
                    <td className="right tnum muted">{pr.pid}</td>
                    <td className="strong ellipsis-cell" title={pr.name}>{pr.name}</td>
                    <td className="hide-sm muted ellipsis-cell">{pr.user}</td>
                    <td className="right tnum"><span className={`num-lvl ${lvl}`}>{fmtPct(pr.cpu, 1)}</span></td>
                    <td className="right tnum">{fmtPct(pr.mem_pct, 1)}</td>
                    <td className="right tnum hide-sm muted">{fmtBytes(pr.rss)}</td>
                    <td className="right tnum hide-sm muted">{pr.threads}</td>
                    <td className="hide-md small" title={STATE_LABEL[pr.state] || pr.state}>{pr.state === 'Z' ? <LevelBadge lvl="warn">zombie</LevelBadge> : STATE_LABEL[pr.state] || pr.state}</td>
                    <td className="hide-md small ellipsis-cell">{pr.container || <span className="faint">–</span>}</td>
                    <td className="hide-sm mono small ellipsis-cell cmd" title={pr.cmd}>{pr.cmd || <span className="faint">[{pr.name}]</span>}</td>
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

type CFilter = 'all' | 'running' | 'stopped' | 'unhealthy'

export function ContainerState({ c }: { c: Container }) {
  if (c.health === 'unhealthy') return <LevelBadge lvl="crit">unhealthy</LevelBadge>
  if (c.state === 'running') return <span className="lvl-badge ok"><Icon name={c.health === 'healthy' ? 'check-circle' : 'play'} size={11} />{c.health === 'starting' ? 'starting' : c.health === 'healthy' ? 'healthy' : 'running'}</span>
  if (c.state === 'restarting') return <LevelBadge lvl="warn">restarting</LevelBadge>
  if (c.state === 'paused') return <span className="lvl-badge off"><Icon name="pause" size={11} />paused</span>
  return <span className="lvl-badge off"><Icon name="power" size={11} />{c.state || 'stopped'}</span>
}

export function ContainerTable({ snap, onLogs }: { snap: Snapshot; onLogs?: (name: string) => void }) {
  const [q, setQ] = useState('')
  const [filter, setFilter] = useState<CFilter>('all')
  const all = snap.containers || []
  const counts = {
    all: all.length,
    running: all.filter(c => c.state === 'running').length,
    stopped: all.filter(c => c.state !== 'running').length,
    unhealthy: all.filter(c => c.health === 'unhealthy').length,
  }
  const n = q.trim().toLowerCase()
  const rows = all.filter(c => (filter === 'all' || (filter === 'running' ? c.state === 'running' : filter === 'stopped' ? c.state !== 'running' : c.health === 'unhealthy'))
    && (!n || `${c.name} ${c.image} ${c.project || ''} ${c.id.slice(0, 12)}`.toLowerCase().includes(n)))
  const sort = useSort<Container>(rows, 'cpu', 'desc')
  const now = Date.now() / 1000
  if (!all.length) return <Panel title="Containers" icon="box"><p className="muted small">No container engine found on this machine (or it can’t read the Docker socket).</p></Panel>
  return (
    <Panel title="Containers" icon="box" flush actions={<span className="muted small tnum">{counts.running} of {counts.all} running</span>}>
      <div className="tbl-tools">
        <Search value={q} onChange={setQ} placeholder="Filter by name, image or project" />
        <Segmented size="sm" label="Show" value={filter} onChange={setFilter} options={[
          { value: 'all', label: `All ${counts.all}` }, { value: 'running', label: `Running ${counts.running}` },
          { value: 'stopped', label: `Stopped ${counts.stopped}` }, { value: 'unhealthy', label: `Unhealthy ${counts.unhealthy}` },
        ]} />
      </div>
      {!sort.rows.length ? <Empty title="No container matches" icon="box" /> : (
        <div className="tbl-wrap">
          <table className="table dense ctr-table">
            <thead><tr>
              <SortTh sort={sort} k="name" label="Name" />
              <SortTh sort={sort} k="project" label="Project" className="hide-md" />
              <SortTh sort={sort} k="state" label="State" />
              <SortTh sort={sort} k="cpu" label="CPU" right />
              <SortTh sort={sort} k="mem_used" label="Memory" right />
              <SortTh sort={sort} k="rx_bps" label="Net ↓ / ↑" right className="hide-sm" />
              <SortTh sort={sort} k="read_bps" label="Disk R / W" right className="hide-md" />
              <SortTh sort={sort} k="started" label="Up" right className="hide-sm" />
              <th className="hide-md">Image</th>
              {onLogs && <th><span className="sr-only">Logs</span></th>}
            </tr></thead>
            <tbody>
              {sort.rows.map(c => (
                <tr key={c.id} className={c.state === 'running' ? '' : 'dim'}>
                  <td className="strong ellipsis-cell" title={`${c.name} · ${c.status}${c.ports ? ` · ${c.ports}` : ''}`}>{c.name}{c.restarts > 0 && <span className="tag warn">{c.restarts} restarts</span>}</td>
                  <td className="hide-md muted small ellipsis-cell">{c.project || '–'}</td>
                  <td><ContainerState c={c} /></td>
                  <td className="right tnum">{c.state === 'running' ? <span className={`num-lvl ${level(c.cpu, 80, 100)}`}>{fmtPct(c.cpu, 1)}</span> : '–'}</td>
                  <td className="right tnum">{c.state === 'running' ? <>{fmtBytes(c.mem_used)}{c.mem_limit > 0 && c.mem_limit < 1e15 && <span className="faint"> / {fmtBytes(c.mem_limit, 0)}</span>}</> : '–'}</td>
                  <td className="right tnum hide-sm small">{c.state === 'running' ? `${fmtRate(c.rx_bps)} / ${fmtRate(c.tx_bps)}` : '–'}</td>
                  <td className="right tnum hide-md small">{c.state === 'running' ? `${fmtRate(c.read_bps)} / ${fmtRate(c.write_bps)}` : '–'}</td>
                  <td className="right tnum hide-sm muted">{c.state === 'running' && c.started ? fmtDuration(now - c.started) : '–'}</td>
                  <td className="hide-md mono small ellipsis-cell" title={c.image}>{c.image}</td>
                  {onLogs && <td className="right"><button type="button" className="btn ghost icon-only xs" aria-label={`Logs of ${c.name}`} title="Logs" onClick={() => onLogs(c.name)}><Icon name="terminal" size={14} /></button></td>}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Panel>
  )
}
