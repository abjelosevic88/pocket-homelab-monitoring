// One server: quicklook tiles, history charts and the live panels.
import { useEffect, useRef, useState } from 'react'
import { Navigate, useParams, useSearchParams } from 'react-router-dom'
import { api, ApiError, V1 } from '../lib/api'
import { useApp } from '../lib/app-context'
import { ago } from '../lib/format'
import type { History, Point, RangeName, Snapshot } from '../lib/types'
import { Icon } from '../components/icons'
import { Select } from '../components/Select'
import { useServerActions } from '../components/ServerDialogs'
import { serverSubtitle, ServerState } from '../components/Status'
import { Shell } from '../components/Shell'
import { Loading, MenuButton, Segmented, Tabs, useIsMobile } from '../components/ui'
import { AlertList } from './Alerts'
import { type ChartData, HistoryCharts } from './server/HistoryCharts'
import { CpuPanel, DiskPanel, FsPanel, MemPanel, NetPanel, NoData, QuickTiles, RaidPanel, SensorsPanel, SystemPanel } from './server/Panels'
import { ContainerTable, ProcessTable } from './server/Tables'
import { LogsView } from './server/Logs'
import { JobsView } from './server/Jobs'

export const RANGES: { value: RangeName; label: string; long: string }[] = [
  { value: 'live', label: 'Live', long: 'Live (last 15 minutes)' }, { value: '1h', label: '1h', long: 'Last hour' }, { value: '6h', label: '6h', long: 'Last 6 hours' },
  { value: '24h', label: '24h', long: 'Last 24 hours' }, { value: '7d', label: '7d', long: 'Last 7 days' }, { value: '30d', label: '30d', long: 'Last 30 days' },
]
type Section = 'overview' | 'processes' | 'containers' | 'jobs' | 'logs' | 'io' | 'system'
const SECTIONS: { id: Section; label: string; icon: string }[] = [
  { id: 'overview', label: 'Overview', icon: 'activity' }, { id: 'processes', label: 'Processes', icon: 'list' },
  { id: 'containers', label: 'Containers', icon: 'box' }, { id: 'jobs', label: 'Jobs', icon: 'clock' }, { id: 'logs', label: 'Logs', icon: 'terminal' }, { id: 'io', label: 'Network & disks', icon: 'network' }, { id: 'system', label: 'System', icon: 'server' },
]

/** Polls `fn` every `ms` while the tab is visible; keeps the last good value. */
function usePoll<T>(fn: () => Promise<T>, ms: number, deps: unknown[]) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<ApiError | Error | null>(null)
  const [loading, setLoading] = useState(true)
  const gen = useRef(0)
  useEffect(() => {
    const my = ++gen.current
    let timer: ReturnType<typeof setTimeout>
    setLoading(true)
    const run = async () => {
      if (!document.hidden) {
        try { const d = await fn(); if (gen.current === my) { setData(d); setError(null) } } catch (e) { if (gen.current === my) setError(e as Error) } finally { if (gen.current === my) setLoading(false) }
      }
      if (ms > 0) timer = setTimeout(run, ms)
    }
    run()
    const onShow = () => { if (!document.hidden) { clearTimeout(timer); run() } }
    document.addEventListener('visibilitychange', onShow)
    return () => { gen.current++; clearTimeout(timer); document.removeEventListener('visibilitychange', onShow) }
  }, deps) // eslint-disable-line react-hooks/exhaustive-deps
  return { data, error, loading, setData }
}

export default function ServerPage() {
  const { id } = useParams()
  const [search, setSearch] = useSearchParams()
  const { servers, serversLoaded, settings, alerts } = useApp()
  const mobile = useIsMobile()
  const actions = useServerActions()
  const sid = Number(id)
  const server = servers.find(s => s.id === sid)
  const range = (RANGES.find(r => r.value === search.get('range'))?.value || settings?.default_range || 'live') as RangeName
  const section = (SECTIONS.find(s => s.id === search.get('tab'))?.id || 'overview') as Section
  const setParam = (k: string, v: string, def: string) => setSearch(p => { const n = new URLSearchParams(p); if (v === def) n.delete(k); else n.set(k, v); return n }, { replace: true })
  const setRange = (r: RangeName) => setParam('range', r, settings?.default_range || 'live')
  const setSection = (s: Section) => setParam('tab', s, 'overview')
  const logsFor = search.get('c') || ''
  const openLogs = (name: string) => setSearch(p => { const n = new URLSearchParams(p); n.set('tab', 'logs'); if (name) n.set('c', name); else n.delete('c'); return n }, { replace: true })

  const interval = Math.max(3, Math.min(server?.interval_s || 5, 10)) * 1000
  const snap = usePoll(() => api.get<Snapshot>(`${V1}/servers/${sid}/snapshot`), server?.enabled === false ? 0 : interval, [sid, interval, server?.enabled])
  const needCharts = section === 'overview'
  const hist = usePoll<ChartData>(async () => {
    if (!needCharts) return { points: [], step: 0, sensors: [] }
    if (range === 'live') {
      const r = await api.get<{ step_s: number; points: Point[] }>(`${V1}/servers/${sid}/live`)
      return { points: r.points || [], step: r.step_s, sensors: [] }
    }
    const h = await api.get<History>(`${V1}/servers/${sid}/history?range=${range}`)
    return { points: h.points || [], step: h.step_s, sensors: h.sensors || [] }
  }, range === 'live' ? interval : 60_000, [sid, range, needCharts, interval])

  // keep the previous render while a new range loads (dimmed), drop it on another server
  const [shownFor, setShownFor] = useState(sid)
  useEffect(() => { if (sid !== shownFor) { snap.setData(null); hist.setData(null); setShownFor(sid) } }, [sid]) // eslint-disable-line react-hooks/exhaustive-deps

  const onKey = (k: string) => {
    const n = Number(k)
    if (n >= 1 && n <= RANGES.length && section === 'overview') { setRange(RANGES[n - 1].value); return true }
    const i = SECTIONS.findIndex(s => s.id === section)
    if (k === ']') { setSection(SECTIONS[(i + 1) % SECTIONS.length].id); return true }
    if (k === '[') { setSection(SECTIONS[(i - 1 + SECTIONS.length) % SECTIONS.length].id); return true }
    return false
  }

  if (!serversLoaded) return <Shell title="…"><Loading /></Shell>
  if (!server) return <Navigate to="/" replace />

  const s = snap.data
  const myAlerts = alerts.filter(a => a.server_id === sid)
  const rangeControl = section !== 'overview' ? undefined : mobile
    ? <div className="range-select"><Select size="sm" value={range} onChange={setRange} options={RANGES.map(r => ({ value: r.value, label: r.long }))} label="Range" width={200} /></div>
    : <Segmented className="view-switch" label="Range" value={range} onChange={setRange} options={RANGES.map((r, i) => ({ value: r.value, label: r.label, title: `${r.long} (${i + 1})` }))} />
  const missing = snap.error instanceof ApiError && snap.error.status === 404

  return (
    <Shell title={<span className="row srv-title"><span className="srv-swatch lg" style={{ ['--c' as string]: server.color }} />{server.name}</span>}
      sub={<span className="row"><ServerState status={server.status} label />{serverSubtitle(server)}</span>}
      controls={rangeControl} loading={snap.loading || hist.loading} onKey={onKey}
      extraCommands={RANGES.map((r, i) => ({ id: 'r' + r.value, label: `Range: ${r.long}`, icon: 'clock', hint: String(i + 1), group: 'This server', run: () => { setSection('overview'); setRange(r.value) } }))}>
      <div className="page-inner">
        <div className="srv-bar">
          <Tabs tabs={SECTIONS.map(x => ({ ...x, label: x.id === 'containers' && s?.containers?.length ? `${x.label} ${s.containers.length}` : x.label }))} value={section} onChange={setSection} />
          <span className="spacer" />
          <MenuButton label={`Options for ${server.name}`} align="right" items={actions.items(server)} width={210}><Icon name="more" size={16} /></MenuButton>
        </div>

        {server.status === 'down' && (
          <div className="callout warn mb"><Icon name="alert" size={15} /><span><b>Stale</b> — last reading {ago(server.last_seen)}.{server.last_error ? ` ${server.last_error}` : ''}</span></div>
        )}
        {server.status === 'paused' && <div className="callout mb"><Icon name="pause" size={15} /><span>Paused — showing the last reading. Resume it from the ⋯ menu.</span></div>}
        {s?.errors?.length ? (
          <details className="quiet-note mb"><summary><Icon name="info" size={13} />Some parts couldn’t be read ({s.errors.length})</summary><ul>{s.errors.map(e => <li key={e} className="mono small">{e}</li>)}</ul></details>
        ) : null}
        {myAlerts.length > 0 && section === 'overview' && <div className="mb"><AlertList alerts={myAlerts} /></div>}

        {!s ? (snap.loading && !missing ? <Loading /> : <NoData server={server} />) : (
          <div className={`srv-content ${snap.loading && !s ? 'refetching' : ''}`}>
            {section === 'overview' && <>
              <QuickTiles snap={s} />
              <HistoryCharts data={hist.data} range={range} loading={hist.loading} />
              <div className="panel-grid">
                <CpuPanel snap={s} />
                <MemPanel snap={s} />
                <SensorsPanel snap={s} />
                <RaidPanel snap={s} />
              </div>
              <FsPanel snap={s} />
            </>}
            {section === 'processes' && <ProcessTable snap={s} />}
            {section === 'containers' && <ContainerTable snap={s} onLogs={server.kind !== 'glances' && server.role === 'owner' ? openLogs : undefined} />}
            {section === 'jobs' && <JobsView server={server} />}
            {section === 'logs' && <LogsView server={server} snap={s} initial={logsFor} onPick={openLogs} />}
            {section === 'io' && <div className="stack"><NetPanel snap={s} /><DiskPanel snap={s} /><FsPanel snap={s} /></div>}
            {section === 'system' && <div className="panel-grid"><SystemPanel snap={s} server={server} /><RaidPanel snap={s} /><SensorsPanel snap={s} /></div>}
          </div>
        )}
      </div>
      {actions.dialogs}
    </Shell>
  )
}
