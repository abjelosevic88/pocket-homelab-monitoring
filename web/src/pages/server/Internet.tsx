// Internet: speed tests of this box's connection (the Ookla CLI) — the newest
// result against the plan, a live view while a test runs, history charts with
// the plan and the slow line, how often it met the minimums, when in the week
// it's slowest, public IP changes and every test. Settings, CSV export and an
// import of Speedtest Tracker's database live in the bar above.
import { useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { api, upload, V1 } from '../../lib/api'
import { useApp } from '../../lib/app-context'
import { ago, fmtBytes, fmtDateTime, plural } from '../../lib/format'
import type { OoklaServer, Server, SpeedInfo, SpeedLive, SpeedSettings, SpeedTest } from '../../lib/types'
import { LineChart, type Series } from '../../components/Chart'
import { Icon } from '../../components/icons'
import { type Option, Select } from '../../components/Select'
import { Confirm, Empty, errMsg, Field, Loading, MenuButton, Modal, Segmented, Switch, download, useAsync, useIsMobile, useToast, type MenuItem } from '../../components/ui'
import { Panel } from './Panels'

type Range = '24h' | '7d' | '30d' | '90d' | '1y' | 'all'
const RANGES: { value: Range; label: string; long: string }[] = [
  { value: '24h', label: '24h', long: 'last 24 hours' }, { value: '7d', label: '7d', long: 'last 7 days' }, { value: '30d', label: '30d', long: 'last 30 days' },
  { value: '90d', label: '90d', long: 'last 90 days' }, { value: '1y', label: '1y', long: 'last year' }, { value: 'all', label: 'All', long: 'all tests' },
]
const EVERY: Option<number>[] = [15, 30, 60, 120, 180, 240, 360, 480, 720, 1440].map(m => ({ value: m, label: m < 60 ? `Every ${m} minutes` : m === 60 ? 'Every hour' : m === 1440 ? 'Once a day' : `Every ${m / 60} hours` }))
const KEEP: Option<number>[] = [{ value: 0, label: 'Forever' }, { value: 90, label: '90 days' }, { value: 365, label: 'A year' }, { value: 730, label: 'Two years' }, { value: 1825, label: 'Five years' }]
const TRIGGER: Record<string, string> = { schedule: 'scheduled', manual: 'by hand', recheck: 'second look', import: 'imported' }

// ---------- numbers ----------

export function fmtMbps(v: number | null | undefined, unit = true): string {
  if (v === null || v === undefined || !isFinite(v)) return '–'
  const n = v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2)
  return unit ? `${n} Mbps` : n
}
const fmtMs = (v: number | null | undefined) => v === null || v === undefined || !isFinite(v) ? '–' : `${v < 10 ? v.toFixed(1) : Math.round(v)} ms`

function quantile(vals: number[], q: number): number | null {
  if (!vals.length) return null
  const s = [...vals].sort((a, b) => a - b)
  const i = (s.length - 1) * q, lo = Math.floor(i), hi = Math.ceil(i)
  return s[lo] + (s[hi] - s[lo]) * (i - lo)
}
const avg = (v: number[]) => v.length ? v.reduce((a, b) => a + b, 0) / v.length : null
const nums = (list: SpeedTest[], f: (t: SpeedTest) => number | null) => list.map(f).filter((v): v is number => v !== null && isFinite(v))

/** How much latency grows when the line is full (bufferbloat), graded like the usual web tests. */
function loadedGrade(t: SpeedTest): { grade: string; extra: number } | null {
  if (t.ping_ms === null) return null
  const worst = Math.max(t.down_latency_ms ?? -1, t.up_latency_ms ?? -1)
  if (worst < 0) return null
  const extra = Math.max(0, worst - t.ping_ms)
  const grade = extra < 5 ? 'A+' : extra < 30 ? 'A' : extra < 60 ? 'B' : extra < 200 ? 'C' : extra < 400 ? 'D' : 'F'
  return { grade, extra }
}

/** What's wrong with a test against the minimums. */
function misses(t: SpeedTest, st: SpeedSettings) {
  return {
    down: !!st.min_down && t.down_mbps !== null && t.down_mbps < st.min_down,
    up: !!st.min_up && t.up_mbps !== null && t.up_mbps < st.min_up,
    ping: !!st.max_ping && t.ping_ms !== null && t.ping_ms > st.max_ping,
  }
}
const slow = (t: SpeedTest, st: SpeedSettings) => { const m = misses(t, st); return m.down || m.up || m.ping }

function until(iso?: string): string {
  if (!iso) return ''
  const s = Math.round((Date.parse(iso) - Date.now()) / 1000)
  if (s < 45) return 'now'
  if (s < 3600) return `in ${Math.round(s / 60)} min`
  const h = Math.floor(s / 3600), m = Math.round((s % 3600) / 60)
  return m && h < 6 ? `in ${h} h ${m} min` : `in ${Math.round(s / 3600)} h`
}

function everyText(st: SpeedSettings) {
  return (EVERY.find(o => o.value === st.every_min)?.label || `Every ${st.every_min} min`).toLowerCase()
}

// ---------- the view ----------

export function InternetView({ server }: { server: Server }) {
  const { version } = useApp()
  const { toast } = useToast()
  const [range, setRange] = useState<Range>('30d')
  const [tick, setTick] = useState(0)
  const info = useAsync(() => api.get<SpeedInfo>(`${V1}/servers/${server.id}/speedtest`), [server.id, version, tick])
  const results = useAsync(() => api.get<SpeedTest[]>(`${V1}/servers/${server.id}/speedtest/results?range=${range}`), [server.id, range, version, tick])
  const [live, setLive] = useState<SpeedLive | null>(null)
  const [dialog, setDialog] = useState<'settings' | 'import' | null>(null)
  const [note, setNote] = useState<SpeedTest | null>(null)
  const [del, setDel] = useState<SpeedTest | null>(null)

  // the countdown moves; a test the scheduler started shows up within the minute
  useEffect(() => { const t = setInterval(() => { if (!document.hidden) setTick(x => x + 1) }, 60_000); return () => clearInterval(t) }, [])
  useEffect(() => { if (info.data?.live) setLive(info.data.live) }, [info.data?.live])

  // while a test runs, follow it closely; when it ends, read everything again
  const following = !!live
  useEffect(() => {
    if (!following) return
    let stop = false
    const run = async () => {
      while (!stop) {
        await new Promise(r => setTimeout(r, 700))
        try {
          const r = await api.get<{ live: SpeedLive | null }>(`${V1}/servers/${server.id}/speedtest/live`)
          if (stop) return
          setLive(r.live)
          if (!r.live) { setTick(x => x + 1); return }
        } catch { /* keep trying */ }
      }
    }
    run()
    return () => { stop = true }
  }, [following, server.id])

  const d = info.data
  const st = d?.settings
  const list = results.data || []
  const owner = !!d?.owner

  const runNow = async () => {
    try {
      const r = await api.post<{ live: SpeedLive | null }>(`${V1}/servers/${server.id}/speedtest/run`, {})
      setLive(r.live || { server_id: server.id, trigger: 'manual', started: new Date().toISOString(), phase: 'starting', progress: 0, mbps: 0, ping_ms: 0, down_mbps: 0, curve: [] })
    } catch (e) { toast(errMsg(e), 'err') }
  }
  const stop = async () => { try { await api.post(`${V1}/servers/${server.id}/speedtest/cancel`) } catch (e) { toast(errMsg(e), 'err') } }
  const reload = () => setTick(x => x + 1)

  if (!d) return info.loading ? <Loading /> : <Empty title="Couldn’t read the speed tests" icon="alert">{info.error}</Empty>
  if (!d.available && !d.count) return <Empty title="Speed tests aren’t available here" icon="wifi">{d.reason}</Empty>
  if (!st) return null

  const menu: MenuItem[] = [
    { label: 'Download as CSV', icon: 'download', onSelect: () => download(`${V1}/servers/${server.id}/speedtest/export.csv`), disabled: !d.count },
  ]
  if (owner) menu.push({ label: 'Import from Speedtest Tracker…', icon: 'upload', onSelect: () => setDialog('import') })

  return (
    <div className="stack">
      <div className="job-bar inet-bar">
        <span className="inet-sched">
          <Icon name={st.enabled ? 'clock' : 'pause'} size={14} className="muted" />
          {st.enabled ? <span>
            Tests {everyText(st)}{d.next_at && <> · next <b title={fmtDateTime(d.next_at)}>{until(d.next_at)}</b></>}
            {d.recheck_at && <> · second look {until(d.recheck_at)}</>}
            {d.put_off && <> · <span className="inet-warn">waiting for the network to calm down</span></>}
            <span className="muted"> · {st.test_server || (st.test_server_id ? `server ${st.test_server_id}` : 'nearest server')}</span>
          </span> : <span className="muted">No scheduled tests</span>}
        </span>
        <span className="spacer" />
        {owner && <button type="button" className="btn sm" onClick={() => setDialog('settings')}><Icon name="settings" size={14} />Settings</button>}
        <MenuButton label="More" align="right" className="btn ghost sm icon-only" items={menu} width={250}><Icon name="more" size={15} /></MenuButton>
        {owner && d.available && (live
          ? <button type="button" className="btn sm danger" onClick={stop}><Icon name="stop" size={13} />Stop test</button>
          : <button type="button" className="btn primary sm" onClick={runNow}><Icon name="play" size={13} />Run a test</button>)}
      </div>

      {!d.available && <div className="callout warn"><Icon name="alert" size={15} /><span>{d.reason}</span></div>}
      {!st.enabled && owner && d.available && (
        <div className="callout info">
          <Icon name="wifi" size={15} />
          <span className="grow">Test the connection on a schedule to see how it holds up through the day and to get an alert when it’s down or slow. <button type="button" className="link-btn" onClick={() => setDialog('settings')}>Set it up</button></span>
        </div>
      )}

      {live && <LiveCard live={live} plan={st.plan_down} />}

      {d.latest && <LatestTiles latest={d.latest} st={st} list={list} />}

      {!d.count ? (
        !live && <Empty title="No tests yet" icon="wifi">{owner && d.available ? 'Run the first one, or import the history Speedtest Tracker collected from the ⋯ menu.' : 'Nothing measured yet.'}</Empty>
      ) : <>
        <div className="job-bar">
          <Segmented size="sm" label="Range" value={range} onChange={setRange} options={RANGES.map(r => ({ value: r.value, label: r.label, title: r.long }))} />
          <span className="spacer" />
          <span className="muted xsmall">{results.loading && !results.data ? 'Loading…' : `${plural(list.length, 'test')} in the ${RANGES.find(r => r.value === range)?.long.replace('all tests', 'whole history')}`}</span>
        </div>
        <SpeedCharts list={list} st={st} loading={results.loading} />
        <div className="panel-grid">
          <Summary list={list} st={st} />
          <SlowestTimes list={list} st={st} />
        </div>
        <IPHistory list={list} />
        <ResultsTable list={list} st={st} owner={owner} onNote={setNote} onDelete={setDel} />
      </>}

      {dialog === 'settings' && <SettingsDialog server={server} info={d} onClose={() => setDialog(null)} onSaved={() => { setDialog(null); reload() }} />}
      {dialog === 'import' && <ImportDialog server={server} onClose={() => setDialog(null)} onDone={reload} />}
      {note && <NoteDialog server={server} test={note} onClose={() => setNote(null)} onSaved={() => { setNote(null); reload() }} />}
      {del && <Confirm title="Delete this test?" message={`${fmtDateTime(del.t)} — ${del.ok ? `${fmtMbps(del.down_mbps)} down, ${fmtMbps(del.up_mbps)} up` : del.error}`} onCancel={() => setDel(null)}
        onConfirm={async () => { const x = del; setDel(null); try { await api.del(`${V1}/servers/${server.id}/speedtest/results/${x.id}`); reload() } catch (e) { toast(errMsg(e), 'err') } }} />}
    </div>
  )
}

// ---------- a test running now ----------

const PHASES = [{ id: 'ping', label: 'Ping' }, { id: 'download', label: 'Download' }, { id: 'upload', label: 'Upload' }] as const

function LiveCard({ live, plan }: { live: SpeedLive; plan: number }) {
  const idx = live.phase === 'ping' ? 0 : live.phase === 'download' ? 1 : live.phase === 'upload' ? 2 : -1
  const overall = idx < 0 ? 0 : idx === 0 ? live.progress * 0.1 : idx === 1 ? 0.1 + live.progress * 0.45 : 0.55 + live.progress * 0.45
  const big = live.phase === 'download' || live.phase === 'upload' ? fmtMbps(live.mbps, false) : live.phase === 'ping' ? fmtMs(live.ping_ms).replace(' ms', '') : '…'
  const unit = live.phase === 'download' || live.phase === 'upload' ? 'Mbps' : live.phase === 'ping' ? 'ms' : ''
  const what = live.phase === 'starting' ? 'Starting' : live.phase === 'connecting' ? 'Connecting' : PHASES[Math.max(0, idx) as 0 | 1 | 2].label
  return (
    <section className="panel inet-live" aria-live="polite">
      <div className="inet-live-main">
        <div className="inet-live-num">
          <span className="inet-live-what"><span className="spinner" style={{ width: 12, height: 12 }} />{what}</span>
          <span className="inet-live-big tnum">{big}<small>{unit}</small></span>
          <span className="muted xsmall">{live.server ? `${live.server.name} · ${live.server.location}` : 'finding a server'}{live.isp ? ` · ${live.isp}` : ''}</span>
        </div>
        <LiveCurve live={live} plan={plan} />
      </div>
      <ol className="inet-steps">
        {PHASES.map((p, i) => (
          <li key={p.id} className={i < idx ? 'done' : i === idx ? 'now' : ''}>
            <Icon name={i < idx ? 'check-circle' : 'circle'} size={13} />{p.label}
            {p.id === 'ping' && idx > 0 && <span className="tnum"> {fmtMs(live.ping_ms)}</span>}
            {p.id === 'download' && idx > 1 && <span className="tnum"> {fmtMbps(live.down_mbps)}</span>}
          </li>
        ))}
      </ol>
      <div className="inet-progress" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(overall * 100)} aria-label="Test progress"><span style={{ width: `${overall * 100}%` }} /></div>
    </section>
  )
}

function LiveCurve({ live, plan }: { live: SpeedLive; plan: number }) {
  const w = 280, h = 70
  const pts = live.curve
  if (pts.length < 2) return <div className="inet-curve" style={{ width: w, height: h }} />
  const t0 = pts[0].ms, t1 = Math.max(pts[pts.length - 1].ms, t0 + 1)
  const top = Math.max(plan || 0, ...pts.map(p => p.mbps)) * 1.08 || 1
  const path = (phase: string) => pts.filter(p => p.phase === phase).map((p, i) => `${i ? 'L' : 'M'}${((p.ms - t0) / (t1 - t0) * w).toFixed(1)},${(h - (p.mbps / top) * (h - 4)).toFixed(1)}`).join('')
  return (
    <svg className="inet-curve" width={w} height={h} role="img" aria-label="Speed during this test">
      {plan > 0 && <line x1={0} x2={w} y1={h - (plan / top) * (h - 4)} y2={h - (plan / top) * (h - 4)} className="inet-curve-plan" />}
      <path d={path('download')} className="inet-curve-line" style={{ stroke: 'var(--series-1)' }} />
      <path d={path('upload')} className="inet-curve-line" style={{ stroke: 'var(--series-2)' }} />
    </svg>
  )
}

// ---------- the newest result ----------

function Tile({ label, icon, value, unit, sub, lvl, children }: { label: string; icon: string; value: React.ReactNode; unit?: string; sub?: React.ReactNode; lvl?: 'ok' | 'warn' | 'crit'; children?: React.ReactNode }) {
  return (
    <div className={`job-tile inet-tile ${lvl || ''}`}>
      <span className="job-tile-label"><Icon name={icon} size={13} />{label}</span>
      <span className="job-tile-value tnum">{value}{unit && <small> {unit}</small>}</span>
      {sub && <span className="job-tile-sub">{sub}</span>}
      {children}
    </div>
  )
}

/** "12% under usual" against the median of the range. */
function versus(v: number | null, usual: number | null, higherBetter: boolean): React.ReactNode {
  if (v === null || usual === null || !usual) return null
  const pct = Math.round((v / usual - 1) * 100)
  if (Math.abs(pct) < 5) return 'about usual'
  const better = higherBetter ? pct > 0 : pct < 0
  return <span className={better ? '' : 'inet-warn'}>{Math.abs(pct)}% {pct > 0 ? 'above' : 'below'} usual</span>
}

function LatestTiles({ latest, st, list }: { latest: SpeedTest; st: SpeedSettings; list: SpeedTest[] }) {
  const ok = list.filter(t => t.ok)
  const usual = { down: quantile(nums(ok, t => t.down_mbps), 0.5), up: quantile(nums(ok, t => t.up_mbps), 0.5), ping: quantile(nums(ok, t => t.ping_ms), 0.5) }
  if (!latest.ok) {
    return (
      <div className="callout danger">
        <Icon name="x-circle" size={15} />
        <span><b>The last test failed</b> {ago(latest.t)} ({TRIGGER[latest.trigger] || latest.trigger}): {latest.error}</span>
      </div>
    )
  }
  const m = misses(latest, st)
  const plan = (v: number | null, p: number) => p > 0 && v !== null ? `${Math.round(v / p * 100)}% of the plan's ${fmtMbps(p)}` : null
  const g = loadedGrade(latest)
  return (
    <div className="inet-tiles">
      <Tile label="Download" icon="download" value={fmtMbps(latest.down_mbps, false)} unit="Mbps" lvl={m.down ? 'crit' : undefined}
        sub={<>{m.down && <><Icon name="alert" size={11} /> under {fmtMbps(st.min_down)} · </>}{plan(latest.down_mbps, st.plan_down) || versus(latest.down_mbps, usual.down, true)}</>} />
      <Tile label="Upload" icon="upload" value={fmtMbps(latest.up_mbps, false)} unit="Mbps" lvl={m.up ? 'crit' : undefined}
        sub={<>{m.up && <><Icon name="alert" size={11} /> under {fmtMbps(st.min_up)} · </>}{plan(latest.up_mbps, st.plan_up) || versus(latest.up_mbps, usual.up, true)}</>} />
      <Tile label="Ping" icon="zap" value={fmtMs(latest.ping_ms).replace(' ms', '')} unit="ms" lvl={m.ping ? 'crit' : undefined}
        sub={<>{m.ping && <><Icon name="alert" size={11} /> over {fmtMs(st.max_ping)} · </>}jitter {fmtMs(latest.jitter_ms)}{latest.loss_pct !== null ? ` · ${latest.loss_pct}% loss` : ''}</>} />
      <Tile label="Under load" icon="activity" value={g ? g.grade : '–'} lvl={g && (g.grade === 'D' || g.grade === 'F') ? 'warn' : undefined}
        sub={g ? `+${Math.round(g.extra)} ms when the line is full` : 'not measured'} />
      <Tile label="Tested" icon="clock" value={<span className="inet-when">{ago(latest.t)}</span>}
        sub={<>{latest.test_server || 'unknown server'}{latest.result_url && <> · <a className="nowrap" href={latest.result_url} target="_blank" rel="noreferrer">result <Icon name="external" size={10} /></a></>}</>} />
    </div>
  )
}

// ---------- charts ----------

function SpeedCharts({ list, st, loading }: { list: SpeedTest[]; st: SpeedSettings; loading: boolean }) {
  const { h12 } = useApp()
  const ok = useMemo(() => list.filter(t => t.ok), [list])
  const xs = useMemo(() => ok.map(t => t.t), [ok])
  const step = st.every_min * 60
  const speed: Series[] = [{ key: 'down', label: 'Download', values: ok.map(t => t.down_mbps) }, { key: 'up', label: 'Upload', values: ok.map(t => t.up_mbps) }]
  const lat: Series[] = [
    { key: 'ping', label: 'Ping (idle)', values: ok.map(t => t.ping_ms) },
    { key: 'dl', label: 'While downloading', values: ok.map(t => t.down_latency_ms) },
    { key: 'ul', label: 'While uploading', values: ok.map(t => t.up_latency_ms) },
  ]
  const guides = [
    ...(st.plan_down ? [{ value: st.plan_down, label: `Plan ${fmtMbps(st.plan_down)}` }] : []),
    ...(st.min_down ? [{ value: st.min_down, label: `Slow under ${fmtMbps(st.min_down)}` }] : []),
  ]
  const failed = list.length - ok.length
  return (
    <div className="chart-grid inet-charts">
      <section className="chart-card">
        <header className="chart-head"><Icon name="wifi" size={15} className="muted" /><h3>Speed</h3><span className="spacer" />{failed > 0 && <span className="chart-now muted">{plural(failed, 'failed test')} not drawn</span>}</header>
        <LineChart xs={xs} series={speed} step={step} h12={h12} loading={loading} height={200} format={v => fmtMbps(v)} tickFormat={v => String(v)} guides={guides} label="Download and upload speed per test" area={false} />
      </section>
      <section className="chart-card">
        <header className="chart-head"><Icon name="zap" size={15} className="muted" /><h3>Latency</h3></header>
        <LineChart xs={xs} series={lat} step={step} h12={h12} loading={loading} height={200} format={fmtMs} tickFormat={v => `${v}`} guides={st.max_ping ? [{ value: st.max_ping, label: `Slow over ${fmtMs(st.max_ping)}` }] : []} label="Ping idle and while the line is busy, per test" area={false} />
      </section>
    </div>
  )
}

// ---------- summary ----------

function Summary({ list, st }: { list: SpeedTest[]; st: SpeedSettings }) {
  const ok = list.filter(t => t.ok)
  const rows: { label: string; vals: number[]; fmt: (v: number | null) => string; higher: boolean }[] = [
    { label: 'Download', vals: nums(ok, t => t.down_mbps), fmt: v => fmtMbps(v), higher: true },
    { label: 'Upload', vals: nums(ok, t => t.up_mbps), fmt: v => fmtMbps(v), higher: true },
    { label: 'Ping', vals: nums(ok, t => t.ping_ms), fmt: fmtMs, higher: false },
    { label: 'Jitter', vals: nums(ok, t => t.jitter_ms), fmt: fmtMs, higher: false },
    { label: 'Under load', vals: ok.map(t => Math.max(t.down_latency_ms ?? -1, t.up_latency_ms ?? -1)).filter(v => v >= 0), fmt: fmtMs, higher: false },
  ]
  const hasMin = !!(st.min_down || st.min_up || st.max_ping)
  const met = ok.filter(t => !slow(t, st)).length
  const failed = list.length - ok.length
  const data = list.reduce((a, t) => a + t.down_bytes + t.up_bytes, 0)
  return (
    <Panel title="How it did" icon="gauge" flush>
      <div className="inet-facts">
        <div><span className="tnum">{list.length ? `${Math.round((ok.length / list.length) * 1000) / 10}%` : '–'}</span><small>tests worked{failed ? ` (${failed} failed)` : ''}</small></div>
        <div className={hasMin && ok.length && met / ok.length < 0.9 ? 'warn' : ''}><span className="tnum">{hasMin && ok.length ? `${Math.round((met / ok.length) * 1000) / 10}%` : '–'}</span><small>{hasMin ? 'met your minimums' : 'set minimums in Settings'}</small></div>
        <div><span className="tnum">{fmtBytes(data, 0)}</span><small>used by the tests</small></div>
      </div>
      <div className="tbl-wrap">
        <table className="table dense inet-sum">
          <thead><tr><th /><th className="right" title="Half the tests were better, half worse">Typical</th><th className="right">Average</th><th className="right" title="One test in ten was this bad or worse">Worst 10%</th><th className="right">Worst</th><th className="right hide-sm">Best</th></tr></thead>
          <tbody>
            {rows.map(r => {
              const lo = r.higher ? quantile(r.vals, 0.1) : quantile(r.vals, 0.9)
              const worst = r.vals.length ? (r.higher ? Math.min(...r.vals) : Math.max(...r.vals)) : null
              const best = r.vals.length ? (r.higher ? Math.max(...r.vals) : Math.min(...r.vals)) : null
              return (
                <tr key={r.label}>
                  <td className="strong">{r.label}</td>
                  <td className="right tnum">{r.fmt(quantile(r.vals, 0.5))}</td>
                  <td className="right tnum">{r.fmt(avg(r.vals))}</td>
                  <td className="right tnum">{r.fmt(lo)}</td>
                  <td className="right tnum">{r.fmt(worst)}</td>
                  <td className="right tnum hide-sm">{r.fmt(best)}</td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
    </Panel>
  )
}

// ---------- when it's slowest ----------

const DAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun']

function SlowestTimes({ list, st }: { list: SpeedTest[]; st: SpeedSettings }) {
  const bh = [1, 2, 3, 4, 6].reduce((best, b) => (b <= Math.max(1, st.every_min / 60) ? b : best), 1)
  const cols = 24 / bh
  const [hover, setHover] = useState<{ d: number; c: number } | null>(null)
  const grid = useMemo(() => {
    const cells: number[][][] = Array.from({ length: 7 }, () => Array.from({ length: cols }, () => []))
    const byCol: number[][] = Array.from({ length: cols }, () => [])
    for (const t of list) {
      if (!t.ok || t.down_mbps === null) continue
      const dt = new Date(t.t * 1000)
      const d = (dt.getDay() + 6) % 7, c = Math.floor(dt.getHours() / bh)
      cells[d][c].push(t.down_mbps)
      byCol[c].push(t.down_mbps)
    }
    return { cells, byCol }
  }, [list, bh, cols])
  const all = nums(list.filter(t => t.ok), t => t.down_mbps)
  const typical = quantile(all, 0.5)
  const medians = grid.cells.map(row => row.map(v => quantile(v, 0.5)))
  // the scale runs from the slowest square to the fastest, so a dip stands out even on a steady line
  const filled = medians.flat().filter((v): v is number => v !== null)
  const lo = Math.min(...filled), hi = Math.max(...filled)
  const shade = (v: number) => `${Math.round(12 + (hi > lo ? (v - lo) / (hi - lo) : 1) * 88)}%`
  const colMed = grid.byCol.map(v => ({ m: quantile(v, 0.5), n: v.length }))
  const worstCol = colMed.reduce((w, x, i) => x.n >= 3 && x.m !== null && (w < 0 || x.m < (colMed[w].m ?? Infinity)) ? i : w, -1)
  const hh = (c: number) => String(c * bh).padStart(2, '0') + ':00'
  const range = (c: number) => `${hh(c)}–${String(((c + 1) * bh) % 24).padStart(2, '0')}:00`
  const hc = hover ? grid.cells[hover.d][hover.c] : null
  if (all.length < 8) return <Panel title="When it’s slowest" icon="calendar"><p className="muted small">A few days of tests are needed to see a pattern.</p></Panel>
  return (
    <Panel title="When it’s slowest" icon="calendar">
      <p className="small inet-insight">
        {worstCol >= 0 && typical && colMed[worstCol].m !== null && colMed[worstCol].m! < typical * 0.95
          ? <>Slowest around <b>{range(worstCol)}</b>: typically {fmtMbps(colMed[worstCol].m)} against {fmtMbps(typical)} overall.</>
          : <>No time of day stands out: typically {fmtMbps(typical)} download.</>}
      </p>
      <div className="inet-heat" style={{ gridTemplateColumns: `34px repeat(${cols}, minmax(0, 1fr))` }} role="table" aria-label="Typical download speed by weekday and time of day" onMouseLeave={() => setHover(null)}>
        <span />
        {Array.from({ length: cols }, (_, c) => <span key={c} className="inet-heat-h">{c % Math.ceil(cols / 8) === 0 ? String(c * bh).padStart(2, '0') : ''}</span>)}
        {DAYS.map((day, d) => [
          <span key={day} className="inet-heat-d">{day}</span>,
          ...medians[d].map((v, c) => (
            <button type="button" key={`${d}-${c}`} className={`inet-heat-c ${v === null ? 'empty' : ''} ${hover?.d === d && hover.c === c ? 'on' : ''}`}
              style={v === null ? undefined : { ['--k' as string]: shade(v) }}
              aria-label={`${day} ${range(c)}: ${v === null ? 'no tests' : fmtMbps(v)}`} onMouseEnter={() => setHover({ d, c })} onFocus={() => setHover({ d, c })} onClick={() => setHover({ d, c })} />
          )),
        ])}
      </div>
      <div className="inet-heat-read small">
        {hover && hc ? (hc.length
          ? <><b>{DAYS[hover.d]} {range(hover.c)}</b> · typically {fmtMbps(quantile(hc, 0.5))} · slowest {fmtMbps(Math.min(...hc))} · {plural(hc.length, 'test')}</>
          : <><b>{DAYS[hover.d]} {range(hover.c)}</b> · no tests</>)
          : <span className="muted">Darker is faster: lightest {fmtMbps(lo)}, darkest {fmtMbps(hi)}. Point at a square for its numbers.</span>}
      </div>
    </Panel>
  )
}

// ---------- public IP ----------

function IPHistory({ list }: { list: SpeedTest[] }) {
  const runs = useMemo(() => {
    const out: { ip: string; isp: string; from: number; to: number; n: number }[] = []
    for (const t of list) {
      if (!t.ok || !t.external_ip) continue
      const last = out[out.length - 1]
      if (last && last.ip === t.external_ip) { last.to = t.t; last.n++; if (t.isp) last.isp = t.isp } else out.push({ ip: t.external_ip, isp: t.isp, from: t.t, to: t.t, n: 1 })
    }
    return out.reverse()
  }, [list])
  if (!runs.length) return null
  const changes = runs.length - 1
  return (
    <Panel title="Public IP address" icon="globe" flush actions={<span className="muted small">{changes ? `changed ${plural(changes, 'time')}` : 'no changes'}</span>}>
      <ul className="inet-ips">
        {runs.slice(0, 8).map((r, i) => (
          <li key={`${r.ip}-${r.from}`}>
            <span className={`job-dot ${i === 0 ? 'ok' : 'off'}`} />
            <span className="mono small">{r.ip}</span>
            <span className="muted small grow ellipsis">{r.isp}</span>
            <span className="muted xsmall nowrap">{i === 0 ? `since ${fmtDateTime(r.from)}` : `${fmtDateTime(r.from)} – ${fmtDateTime(r.to)}`} · {plural(r.n, 'test')}</span>
          </li>
        ))}
        {runs.length > 8 && <li className="muted xsmall">…and {runs.length - 8} earlier</li>}
      </ul>
    </Panel>
  )
}

// ---------- every test ----------

function ResultsTable({ list, st, owner, onNote, onDelete }: { list: SpeedTest[]; st: SpeedSettings; owner: boolean; onNote: (t: SpeedTest) => void; onDelete: (t: SpeedTest) => void }) {
  const mobile = useIsMobile()
  const [n, setN] = useState(50)
  const [only, setOnly] = useState<'all' | 'slow' | 'failed'>('all')
  const rows = useMemo(() => [...list].reverse().filter(t => only === 'all' || (only === 'failed' ? !t.ok : t.ok && slow(t, st))), [list, only, st])
  const counts = { slow: list.filter(t => t.ok && slow(t, st)).length, failed: list.filter(t => !t.ok).length }
  const bad = (on: boolean, v: string) => on ? <span className="inet-bad"><Icon name="alert" size={11} />{v}</span> : v
  return (
    <Panel title="Tests" icon="list" flush actions={
      <div className="chips">
        {([['all', `All ${list.length}`], ['slow', `Slow ${counts.slow}`], ['failed', `Failed ${counts.failed}`]] as ['all' | 'slow' | 'failed', string][]).map(([k, l]) => (
          <button key={k} type="button" className={`chip ${only === k ? 'active' : ''}`} onClick={() => setOnly(k)}>{l}</button>
        ))}
      </div>}>
      {!rows.length ? <Empty title="Nothing here" icon="check-circle">{only === 'failed' ? 'No test failed in this range.' : 'No test was under the minimums in this range.'}</Empty> : (
        <div className="tbl-wrap">
          <table className="table dense inet-table">
            <thead><tr>
              <th>When</th>
              <th className="right">Download</th>
              <th className="right">Upload</th>
              <th className="right">Ping</th>
              <th className="right hide-sm">Jitter</th>
              <th className="right hide-md" title="Latency while the line is full">Under load</th>
              <th className="hide-md">Server</th>
              <th className="hide-lg">Public IP</th>
              <th aria-label="Actions" />
            </tr></thead>
            <tbody>
              {rows.slice(0, n).map(t => {
                const m = misses(t, st)
                const busy = t.busy_mbps !== null && t.busy_mbps >= 5
                return (
                  <tr key={t.id} className={t.ok ? '' : 'inet-failed'}>
                    <td className="nowrap small">
                      <span title={new Date(t.t * 1000).toLocaleString()}>{fmtDateTime(t.t)}</span>
                      {t.trigger !== 'schedule' && <span className="tag">{TRIGGER[t.trigger] || t.trigger}</span>}
                      {busy && <span className="tag" title={`${fmtMbps(t.busy_mbps)} of other traffic on this box when the test started; the result may be low because of it`}>busy</span>}
                      {t.note && <div className="faint xsmall ellipsis inet-note" title={t.note}>{t.note}</div>}
                    </td>
                    {t.ok ? <>
                      <td className="right tnum">{bad(m.down, fmtMbps(t.down_mbps, !mobile))}</td>
                      <td className="right tnum">{bad(m.up, fmtMbps(t.up_mbps, !mobile))}</td>
                      <td className="right tnum">{bad(m.ping, fmtMs(t.ping_ms))}</td>
                      <td className="right tnum hide-sm">{fmtMs(t.jitter_ms)}</td>
                      <td className="right tnum hide-md">{(() => { const g = loadedGrade(t); return g ? <span title={`+${Math.round(g.extra)} ms`}>{g.grade}</span> : '–' })()}</td>
                      <td className="hide-md small ellipsis inet-srv" title={t.test_location}>{t.test_server || '–'}</td>
                      <td className="hide-lg mono xsmall">{t.external_ip || '–'}</td>
                    </> : <td colSpan={7} className="small inet-err"><Icon name="x-circle" size={12} /> {t.error || 'failed'}</td>}
                    <td className="right nowrap">
                      {t.result_url && <a className="btn ghost xs icon-only" href={t.result_url} target="_blank" rel="noreferrer" title="Open on speedtest.net" aria-label="Open on speedtest.net"><Icon name="external" size={13} /></a>}
                      {owner && <MenuButton label="Test options" align="right" className="btn ghost xs icon-only" width={180} items={[
                        { label: t.note ? 'Edit note…' : 'Add a note…', icon: 'edit', onSelect: () => onNote(t) },
                        { label: 'Delete', icon: 'trash', danger: true, onSelect: () => onDelete(t) },
                      ]}><Icon name="more" size={14} /></MenuButton>}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
          {rows.length > n && <div className="inet-more"><button type="button" className="btn sm" onClick={() => setN(n + 100)}>Show more ({rows.length - n} left)</button></div>}
        </div>
      )}
    </Panel>
  )
}

// ---------- dialogs ----------

function minutesToTime(m: number) { return `${String(Math.floor(m / 60) % 24).padStart(2, '0')}:${String(m % 60).padStart(2, '0')}` }

function SettingsDialog({ server, info, onClose, onSaved }: { server: Server; info: SpeedInfo; onClose: () => void; onSaved: () => void }) {
  const { toast } = useToast()
  const [f, setF] = useState<SpeedSettings>({ ...info.settings })
  const [first, setFirst] = useState(minutesToTime(info.settings.offset_min))
  const [busy, setBusy] = useState(false)
  const servers = useAsync(() => api.get<OoklaServer[]>(`${V1}/servers/${server.id}/speedtest/test-servers`), [server.id])
  const firstRun = useRef(!info.settings.enabled)
  const offset = (() => { const [h, m] = first.split(':').map(Number); return isFinite(h) && isFinite(m) ? (h * 60 + m) % f.every_min : 0 })()
  const times = Array.from({ length: Math.min(1440 / f.every_min, 96) }, (_, i) => minutesToTime(offset + i * f.every_min))
  const num = (k: keyof SpeedSettings) => ({
    value: f[k] ? String(f[k]) : '', inputMode: 'decimal' as const, placeholder: '—',
    onChange: (e: React.ChangeEvent<HTMLInputElement>) => setF({ ...f, [k]: Math.max(0, parseFloat(e.target.value.replace(',', '.')) || 0) }),
  })
  const serverOpts: Option<number>[] = [{ value: 0, label: 'Nearest (picked each time)', description: 'Results can jump between servers' }]
  for (const s of servers.data || []) serverOpts.push({ value: s.id, label: s.name, description: `${s.location}, ${s.country} · ${s.id}` })
  if (f.test_server_id && !serverOpts.some(o => o.value === f.test_server_id)) serverOpts.push({ value: f.test_server_id, label: f.test_server || `Server ${f.test_server_id}` })

  const save = async () => {
    setBusy(true)
    try {
      const name = (servers.data || []).find(s => s.id === f.test_server_id)
      await api.put(`${V1}/servers/${server.id}/speedtest/settings`, { ...f, offset_min: offset, test_server: f.test_server_id ? (name ? `${name.name} (${name.location})` : f.test_server) : '' })
      toast(f.enabled && firstRun.current ? 'Scheduled. Alert rules for failed and slow tests were added.' : 'Saved')
      onSaved()
    } catch (e) { toast(errMsg(e), 'err') } finally { setBusy(false) }
  }
  return (
    <Modal title="Speed test settings" size="lg" onClose={onClose} footer={<>
      <button className="btn" onClick={onClose}>Cancel</button>
      <button className="btn primary" disabled={busy} onClick={save}>Save</button>
    </>}>
      <div className="stack">
        <label className="logs-check inet-switch"><Switch checked={f.enabled} onChange={v => setF({ ...f, enabled: v })} label="Test on a schedule" /><span><b>Test on a schedule</b><br /><span className="muted xsmall">Tests use Ookla’s Speedtest CLI; turning them on accepts its <a href="https://www.speedtest.net/about/eula" target="_blank" rel="noreferrer">license</a> and <a href="https://www.speedtest.net/about/privacy" target="_blank" rel="noreferrer">privacy policy</a>. A test moves about {fmtBytes(Math.max(200, (f.plan_down + f.plan_up) * 2.6) * 1e6, 0)}.</span></span></label>
        <div className="two-col">
          <Field label="How often" group><Select value={f.every_min} onChange={v => setF({ ...f, every_min: v })} options={EVERY} label="How often" /></Field>
          <Field label="First test of the day" help="Off the hour is quieter; other jobs like to start at :00."><input type="time" value={first} onChange={e => setFirst(e.target.value)} /></Field>
        </div>
        <p className="muted xsmall inet-times">Runs at {times.length > 12 ? `${times.slice(0, 6).join(', ')} … ${times.slice(-2).join(', ')}` : times.join(', ')}. A failed or slow scheduled test gets a second look 10 minutes later.</p>
        <Field label="Test server" group help={servers.error ? `Couldn’t list servers: ${servers.error}` : 'One fixed server keeps the results comparable over time.'}>
          <Select value={f.test_server_id} onChange={v => setF({ ...f, test_server_id: v })} options={serverOpts} label="Test server" searchable />
        </Field>
        <h4 className="inet-h">Your plan</h4>
        <div className="two-col">
          <Field label="Download" help="Mbps you pay for"><input {...num('plan_down')} /></Field>
          <Field label="Upload" help="Mbps"><input {...num('plan_up')} /></Field>
        </div>
        <h4 className="inet-h">Counts as slow</h4>
        <div className="three-col">
          <Field label="Download under" help="Mbps"><input {...num('min_down')} /></Field>
          <Field label="Upload under" help="Mbps"><input {...num('min_up')} /></Field>
          <Field label="Ping over" help="ms"><input {...num('max_ping')} /></Field>
        </div>
        <p className="muted xsmall">Marks slow tests in red and draws the line on the chart. {firstRun.current ? 'When you turn tests on, alert rules start from these numbers.' : <>Alerts have their own limits in <Link to={`/settings/rules?server=${server.id}`}>Alert rules</Link>.</>}</p>
        <div className="two-col">
          <Field label="Put a test off while the box moves more than" help={`Mbps of other traffic (up to 30 min; empty = never). Now: ${info.busy_mbps >= 0 ? fmtMbps(info.busy_mbps) : 'unknown'}. LAN traffic counts too.`}><input {...num('busy_mbps')} /></Field>
          <Field label="Keep tests" group><Select value={f.keep_days} onChange={v => setF({ ...f, keep_days: v })} options={KEEP} label="Keep tests" /></Field>
        </div>
      </div>
    </Modal>
  )
}

function ImportDialog({ server, onClose, onDone }: { server: Server; onClose: () => void; onDone: () => void }) {
  const { toast } = useToast()
  const [file, setFile] = useState<File | null>(null)
  const [busy, setBusy] = useState(false)
  const [res, setRes] = useState<{ added: number; skipped: number; failed: number } | null>(null)
  const go = async () => {
    if (!file) return
    setBusy(true)
    try {
      const r = await upload<{ added: number; skipped: number; failed: number }>(`${V1}/servers/${server.id}/speedtest/import`, 'file', file)
      setRes(r)
      onDone()
    } catch (e) { toast(errMsg(e), 'err') } finally { setBusy(false) }
  }
  return (
    <Modal title="Import from Speedtest Tracker" onClose={onClose} footer={res ? <button className="btn primary" onClick={onClose}>Done</button> : <>
      <button className="btn" onClick={onClose}>Cancel</button>
      <button className="btn primary" disabled={!file || busy} onClick={go}>{busy ? 'Importing…' : 'Import'}</button>
    </>}>
      {res ? (
        <div className="callout success"><Icon name="check-circle" size={15} /><span>Imported {plural(res.added, 'test')}{res.failed ? ` (${res.failed} of them failed tests)` : ''}.{res.skipped ? ` ${plural(res.skipped, 'test was', 'tests were')} already here.` : ''}</span></div>
      ) : (
        <div className="stack">
          <p className="small">Pick Speedtest Tracker’s <span className="mono">database.sqlite</span> (in its config folder). Stop the container first so the file is complete. Importing twice is safe: tests already here are skipped.</p>
          <input type="file" accept=".sqlite,.db,application/x-sqlite3,application/vnd.sqlite3" onChange={e => setFile(e.target.files?.[0] || null)} />
        </div>
      )}
    </Modal>
  )
}

function NoteDialog({ server, test, onClose, onSaved }: { server: Server; test: SpeedTest; onClose: () => void; onSaved: () => void }) {
  const { toast } = useToast()
  const [v, setV] = useState(test.note)
  const save = async () => {
    try { await api.put(`${V1}/servers/${server.id}/speedtest/results/${test.id}`, { note: v }); onSaved() } catch (e) { toast(errMsg(e), 'err') }
  }
  return (
    <Modal title={`Note for ${fmtDateTime(test.t)}`} size="sm" onClose={onClose} footer={<><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" onClick={save}>Save</button></>}>
      <Field label="Note" help="Router rebooted, ISP outage, on Wi-Fi…"><textarea rows={3} maxLength={500} value={v} onChange={e => setV(e.target.value)} autoFocus /></Field>
    </Modal>
  )
}

// ---------- on the server's overview ----------

/** One line on the Overview tab: the newest test, linking to the Internet tab. */
export function InternetStrip({ server, onOpen }: { server: Server; onOpen: () => void }) {
  const { version } = useApp()
  const info = useAsync(() => api.get<SpeedInfo>(`${V1}/servers/${server.id}/speedtest`).catch(() => null), [server.id, version])
  const d = info.data
  if (!d?.latest) return null
  const t = d.latest
  const s = slow(t, d.settings)
  return (
    <button type="button" className={`inet-strip ${!t.ok || s ? 'bad' : ''}`} onClick={onOpen}>
      <Icon name="wifi" size={14} className="muted" />
      <span className="strong">Internet</span>
      {t.ok ? <span className="tnum">↓ {fmtMbps(t.down_mbps)} · ↑ {fmtMbps(t.up_mbps)} · {fmtMs(t.ping_ms)}</span> : <span className="inet-bad"><Icon name="x-circle" size={12} />last test failed</span>}
      {t.ok && s && <span className="inet-bad"><Icon name="alert" size={12} />slow</span>}
      <span className="muted xsmall">{ago(t.t)}</span>
      <span className="spacer" />
      {d.live ? <span className="muted xsmall">testing now…</span> : d.next_at && <span className="muted xsmall hide-sm">next {until(d.next_at)}</span>}
      <Icon name="chevron-right" size={14} className="muted" />
    </button>
  )
}
