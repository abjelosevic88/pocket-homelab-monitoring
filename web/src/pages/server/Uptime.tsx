// Uptime: checks of websites (status code, keyword, certificate), TCP ports,
// DNS lookups and push heartbeats from scripts, run from the machine the app
// runs on. Each row shows where a check stands and its newest results; a row
// opens response times, uptime over a range and every time it went down. The
// public status page and an import of Uptime Kuma's monitors live in the bar.
import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { api, upload, V1 } from '../../lib/api'
import { useApp } from '../../lib/app-context'
import { ago, fmtDateTime, plural } from '../../lib/format'
import type { Beat, Check, CheckHistory, CheckKind, ChecksInfo, Server, StatusPage } from '../../lib/types'
import { LineChart } from '../../components/Chart'
import { Icon } from '../../components/icons'
import { type Option, Select } from '../../components/Select'
import { Confirm, CopyField, Empty, errMsg, Field, Loading, MenuButton, Modal, Segmented, Switch, useAsync, useIsMobile, useToast, type MenuItem } from '../../components/ui'
import { Panel } from './Panels'

const KINDS: { value: CheckKind; label: string; icon: string; help: string }[] = [
  { value: 'http', label: 'Website', icon: 'globe', help: 'Asks a URL and expects a good status code, optionally a word on the page. Also watches the HTTPS certificate.' },
  { value: 'tcp', label: 'Port', icon: 'network', help: 'Opens a TCP connection to a host and port.' },
  { value: 'dns', label: 'DNS', icon: 'server', help: 'Looks a name up, optionally at a DNS server of your choice.' },
  { value: 'push', label: 'Push', icon: 'zap', help: 'Waits for a script or app to call its URL; no call within the interval counts as a failure.' },
]
const KIND_ICON: Record<string, string> = Object.fromEntries(KINDS.map(k => [k.value, k.icon]))
const EVERY: Option<number>[] = [20, 30, 60, 120, 300, 600, 900, 1800, 3600].map(s => ({ value: s, label: s < 60 ? `Every ${s} seconds` : s === 60 ? 'Every minute' : s < 3600 ? `Every ${s / 60} minutes` : 'Every hour' }))
const PUSH_EVERY: Option<number>[] = [60, 300, 600, 900, 1800, 3600, 7200, 21600, 43200, 90000, 100000, 172800, 604800].map(s => ({ value: s, label: s < 3600 ? `${s / 60} minutes` : s < 86400 ? `${s / 3600} hours` : s === 90000 ? '25 hours (a daily job)' : s === 100000 ? '~28 hours' : `${Math.round(s / 86400)} days` }))
const RETRIES: Option<number>[] = [0, 1, 2, 3, 5].map(n => ({ value: n, label: n === 0 ? 'Down at the first failure' : `After ${n + 1} failures in a row` }))
const DNS_TYPES: Option<string>[] = ['A', 'AAAA', 'CNAME', 'MX', 'NS', 'TXT'].map(t => ({ value: t, label: t }))
type Range = '24h' | '7d' | '30d' | '90d' | '1y'
const RANGES: { value: Range; label: string; long: string }[] = [
  { value: '24h', label: '24h', long: 'last 24 hours' }, { value: '7d', label: '7d', long: 'last 7 days' }, { value: '30d', label: '30d', long: 'last 30 days' },
  { value: '90d', label: '90d', long: 'last 90 days' }, { value: '1y', label: '1y', long: 'last year' },
]

// ---------- small bits ----------

export const STATUS_WORD = ['Down', 'Up', 'Retrying', 'Maintenance']
const stClass = (c: { enabled?: boolean; status: number; last_at?: number }) => c.enabled === false ? 'paused' : c.last_at === 0 ? 'waiting' : ['down', 'up', 'pending', 'maint'][c.status] || 'waiting'

export function fmtUptime(v: number | null | undefined): string {
  if (v === null || v === undefined) return '–'
  const p = v * 100
  if (p >= 99.995) return '100%'
  return `${p >= 99 ? p.toFixed(2) : p.toFixed(1)}%`
}
const fmtMs = (v: number | null | undefined) => v === null || v === undefined ? '–' : v < 10 ? `${v.toFixed(1)} ms` : `${Math.round(v)} ms`

function target(c: Check): string {
  switch (c.kind) {
    case 'http': return c.target.replace(/^https?:\/\//, '').replace(/\/$/, '') + (c.keyword ? ` · ${c.invert_keyword ? 'without' : 'with'} “${c.keyword}”` : '')
    case 'tcp': return `${c.target}:${c.port}`
    case 'dns': return `${c.target} ${c.dns_type}${c.dns_server ? ` @ ${c.dns_server}` : ''}`
    case 'push': return `heartbeat every ${PUSH_EVERY.find(o => o.value === c.interval_s)?.label || `${Math.round(c.interval_s / 60)} min`}`
  }
  return c.target
}

function certDays(c: Check): number | null {
  if (c.kind !== 'http' || !c.cert_expires || c.ignore_tls || !c.target.startsWith('https')) return null
  return Math.floor((c.cert_expires * 1000 - Date.now()) / 86_400_000)
}

/** The newest results as thin bars, oldest left (Uptime Kuma's beat bar). */
export function BeatBar({ beats, n = 30 }: { beats: { s: number; t: number; ms?: number | null; msg?: string }[]; n?: number }) {
  const list = beats.slice(-n)
  const pad = Math.max(0, n - list.length)
  return (
    <span className="beat-bar" aria-hidden>
      {Array.from({ length: pad }, (_, i) => <i key={'p' + i} className="bb-none" />)}
      {list.map(b => <i key={b.t} className={['bb-down', 'bb-up', 'bb-pend', 'bb-maint'][b.s] || 'bb-none'} title={b.t < 1e9 ? undefined : `${new Date(b.t * 1000).toLocaleString()} · ${STATUS_WORD[b.s] || ''}${b.ms != null ? ` · ${fmtMs(b.ms)}` : ''}${b.msg ? ` · ${b.msg}` : ''}`} />)}
    </span>
  )
}

function Dot({ c }: { c: Check }) {
  const cls = stClass(c)
  const word = cls === 'paused' ? 'Paused' : cls === 'waiting' ? 'Not checked yet' : STATUS_WORD[c.status]
  return <span className={`up-dot ${cls}`} title={word} role="img" aria-label={word} />
}

// ---------- the view ----------

export function UptimeView({ server, initial }: { server: Server; initial?: number }) {
  const { version } = useApp()
  const { toast } = useToast()
  const [tick, setTick] = useState(0)
  const info = useAsync(() => api.get<ChecksInfo>(`${V1}/servers/${server.id}/checks`), [server.id, version, tick])
  const [open, setOpen] = useState<number | null>(initial || null)
  const [edit, setEdit] = useState<Partial<Check> | null>(null)
  const [del, setDel] = useState<Check | null>(null)
  const [dialog, setDialog] = useState<'import' | 'page' | null>(null)
  const [only, setOnly] = useState<'all' | 'down' | 'paused'>('all')
  const [q, setQ] = useState('')

  useEffect(() => { const t = setInterval(() => { if (!document.hidden) setTick(x => x + 1) }, 20_000); return () => clearInterval(t) }, [])
  const reload = () => setTick(x => x + 1)

  const d = info.data
  const checks = d?.checks || []
  const counts = useMemo(() => ({
    up: checks.filter(c => c.enabled && c.status === 1).length, down: checks.filter(c => c.enabled && c.status === 0).length,
    pending: checks.filter(c => c.enabled && c.status === 2 && c.last_at > 0).length, paused: checks.filter(c => !c.enabled).length,
  }), [checks])
  const shown = checks.filter(c => (only === 'all' || (only === 'down' ? c.enabled && (c.status === 0 || c.status === 2) && c.last_at > 0 : !c.enabled)) &&
    (!q || `${c.name} ${c.target} ${c.group}`.toLowerCase().includes(q.toLowerCase())))
  const groups = useMemo(() => {
    const m = new Map<string, Check[]>()
    for (const c of shown) { const g = c.group || ''; m.set(g, [...(m.get(g) || []), c]) }
    return [...m.entries()]
  }, [shown])

  if (!d) return info.loading ? <Loading /> : <Empty title="Couldn’t read the checks" icon="alert">{info.error}</Empty>
  if (!d.available && !checks.length) return <Empty title="Uptime checks aren’t available here" icon="globe">{d.reason}</Empty>
  const owner = d.owner

  const act = async (c: Check, body: Partial<Check>, msg?: string) => {
    try { await api.put(`${V1}/servers/${server.id}/checks/${c.id}`, body); if (msg) toast(msg); reload() } catch (e) { toast(errMsg(e), 'err') }
  }
  const runNow = async (c: Check) => {
    try {
      const r = await api.post<Check>(`${V1}/servers/${server.id}/checks/${c.id}/run`)
      toast(`${r.name}: ${STATUS_WORD[r.status]} — ${r.last_msg}`, r.status === 1 ? undefined : 'err')
      reload()
    } catch (e) { toast(errMsg(e), 'err') }
  }
  const rowMenu = (c: Check): MenuItem[] => [
    { label: 'Edit…', icon: 'edit', onSelect: () => setEdit(c) },
    ...(c.kind !== 'push' && c.enabled ? [{ label: 'Check now', icon: 'refresh', onSelect: () => runNow(c) }] : []),
    { label: c.enabled ? 'Pause' : 'Resume', icon: c.enabled ? 'pause' : 'play', onSelect: () => act(c, { enabled: !c.enabled }) },
    { label: c.notify ? 'Mute alerts' : 'Unmute alerts', icon: c.notify ? 'bell-off' : 'bell', onSelect: () => act(c, { notify: !c.notify }, c.notify ? `${c.name} won’t send alerts` : undefined) },
    { label: c.public ? 'Hide from the status page' : 'Show on the status page', icon: c.public ? 'eye-off' : 'eye', onSelect: () => act(c, { public: !c.public }) },
    { label: 'Duplicate…', icon: 'copy', onSelect: () => setEdit({ ...c, id: undefined, name: `${c.name} (copy)` }) },
    'sep',
    { label: 'Delete…', icon: 'trash', danger: true, onSelect: () => setDel(c) },
  ]
  const menu: MenuItem[] = [
    { label: 'Status page…', icon: 'globe', onSelect: () => setDialog('page'), disabled: !owner },
    { label: 'Import from Uptime Kuma…', icon: 'upload', onSelect: () => setDialog('import'), disabled: !owner },
    'sep',
    { label: 'Alert rules', icon: 'bell', onSelect: () => { window.location.href = `/settings/rules?server=${server.id}` } },
  ]
  const page = d.status_page

  return (
    <div className="stack">
      <div className="job-bar">
        <span className="up-sum">
          {!checks.length ? <span className="muted">No checks yet</span> : <>
            <span><b className="tnum">{counts.up}</b> up</span>
            {counts.down > 0 && <span className="up-bad"><b className="tnum">{counts.down}</b> down</span>}
            {counts.pending > 0 && <span className="up-warn"><b className="tnum">{counts.pending}</b> retrying</span>}
            {counts.paused > 0 && <span className="muted"><b className="tnum">{counts.paused}</b> paused</span>}
          </>}
          {page?.enabled && <Link className="muted xsmall nowrap" to={`/status/${page.slug}`} target="_blank"><Icon name="globe" size={12} /> /status/{page.slug}</Link>}
        </span>
        <span className="spacer" />
        <MenuButton label="More" align="right" className="btn ghost sm icon-only" items={menu} width={240}><Icon name="more" size={15} /></MenuButton>
        {owner && <button type="button" className="btn primary sm" onClick={() => setEdit({})}><Icon name="plus" size={14} />Add check</button>}
      </div>

      {!checks.length ? (
        <Empty title="Nothing watched yet" icon="globe">
          {owner ? <>Add a check for a website, a port, a DNS name or a script’s heartbeat — or <button type="button" className="link-btn" onClick={() => setDialog('import')}>import your Uptime Kuma monitors</button>.</> : 'The owner hasn’t added any checks.'}
        </Empty>
      ) : <>
        {counts.down > 0 && only === 'all' && !q && (
          <div className="callout danger">
            <Icon name="x-circle" size={15} />
            <span className="grow">{checks.filter(c => c.enabled && c.status === 0).map((c, i) => <span key={c.id}>{i > 0 && ', '}<button type="button" className="link-btn" onClick={() => setOpen(c.id)}>{c.name}</button></span>)} {counts.down === 1 ? 'is' : 'are'} down.</span>
          </div>
        )}
        <div className="job-bar">
          <div className="chips">
            {([['all', `All ${checks.length}`], ['down', `Failing ${counts.down + counts.pending}`], ['paused', `Paused ${counts.paused}`]] as ['all' | 'down' | 'paused', string][]).map(([k, l]) => (
              <button key={k} type="button" className={`chip ${only === k ? 'active' : ''}`} onClick={() => setOnly(k)}>{l}</button>
            ))}
          </div>
          <span className="spacer" />
          <input className="up-search" type="search" placeholder="Filter…" value={q} onChange={e => setQ(e.target.value)} aria-label="Filter checks" />
        </div>
        {!shown.length ? <Empty title="Nothing here" icon="check-circle">{only === 'down' ? 'Every check passes.' : 'No check matches.'}</Empty> :
          groups.map(([g, list]) => (
            <Panel key={g || '-'} title={g || (groups.length > 1 ? 'Other' : 'Checks')} icon="globe" flush actions={<span className="muted xsmall">{plural(list.length, 'check')}</span>}>
              <ul className="up-list">
                {list.map(c => (
                  <li key={c.id} className={`up-row st-${stClass(c)} ${open === c.id ? 'open' : ''}`}>
                    <div className="up-line" role="button" tabIndex={0} aria-expanded={open === c.id} onClick={() => setOpen(open === c.id ? null : c.id)} onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); setOpen(open === c.id ? null : c.id) } }}>
                      <Dot c={c} />
                      <span className="up-name">
                        <span className="strong ellipsis">{c.name}{!c.notify && <Icon name="bell-off" size={11} className="muted up-ico" />}{c.public && <Icon name="eye" size={11} className="muted up-ico" />}</span>
                        <span className="muted xsmall ellipsis"><Icon name={KIND_ICON[c.kind]} size={10} /> {target(c)}</span>
                      </span>
                      <span className="up-msg muted xsmall ellipsis hide-md" title={c.last_msg}>{c.enabled ? (c.last_at ? c.last_msg : 'waiting for the first result') : 'paused'}</span>
                      <BeatBar beats={c.beats} />
                      <span className="up-num tnum hide-sm" title="Response time of the newest result">{c.kind === 'push' ? (c.last_at ? ago(c.last_at) : '–') : fmtMs(c.last_ms)}</span>
                      <span className={`up-num tnum ${c.uptime_24h !== null && c.uptime_24h < 0.99 ? 'up-bad' : ''}`} title="Uptime, last 24 hours">{fmtUptime(c.uptime_24h)}</span>
                      {owner ? <span onClick={e => e.stopPropagation()}><MenuButton label={`Options for ${c.name}`} align="right" className="btn ghost sm icon-only" items={() => rowMenu(c)} width={230}><Icon name="more" size={15} /></MenuButton></span> : <span />}
                    </div>
                    {open === c.id && <CheckDetail server={server} check={c} info={d} onEdit={owner ? () => setEdit(c) : undefined} onRun={owner && c.kind !== 'push' ? () => runNow(c) : undefined} />}
                  </li>
                ))}
              </ul>
            </Panel>
          ))}
      </>}

      {edit && <CheckDialog server={server} check={edit} groups={[...new Set(checks.map(c => c.group).filter(Boolean))]} onClose={() => setEdit(null)} onSaved={c => { setEdit(null); setOpen(c.id); reload() }} />}
      {dialog === 'import' && <KumaImportDialog server={server} onClose={() => setDialog(null)} onDone={reload} />}
      {dialog === 'page' && <StatusPageDialog server={server} page={page} checks={checks} onClose={() => setDialog(null)} onSaved={() => { setDialog(null); reload() }} />}
      {del && <Confirm title={`Delete “${del.name}”?`} message="Its history goes with it. Scripts still calling a push URL will get “not found”." onCancel={() => setDel(null)}
        onConfirm={async () => { const x = del; setDel(null); try { await api.del(`${V1}/servers/${server.id}/checks/${x.id}`); if (open === x.id) setOpen(null); reload() } catch (e) { toast(errMsg(e), 'err') } }} />}
    </div>
  )
}

// ---------- one check, opened ----------

function CheckDetail({ server, check: c, info, onEdit, onRun }: { server: Server; check: Check; info: ChecksInfo; onEdit?: () => void; onRun?: () => void }) {
  const { h12, version } = useApp()
  const [range, setRange] = useState<Range>('24h')
  const hist = useAsync(() => api.get<CheckHistory>(`${V1}/servers/${server.id}/checks/${c.id}/history?range=${range}`), [server.id, c.id, range, version, c.last_at])
  const h = hist.data
  const days = certDays(c)
  const pushURL = c.push_token ? `${info.push_base}${c.push_token}` : ''
  const xs = (h?.points || []).map(p => p.t)
  return (
    <div className="up-detail">
      <div className="up-tiles">
        <div className={`job-tile ${c.enabled && c.status === 0 ? 'crit' : ''}`}>
          <span className="job-tile-label"><Icon name="activity" size={13} />Status</span>
          <span className="job-tile-value">{!c.enabled ? 'Paused' : c.last_at ? STATUS_WORD[c.status] : 'Waiting'}</span>
          <span className="job-tile-sub">{c.changed_at ? `since ${ago(c.changed_at).replace(' ago', '')}` : c.last_at ? `checked ${ago(c.last_at)}` : ''}</span>
        </div>
        <div className="job-tile">
          <span className="job-tile-label"><Icon name="trending-up" size={13} />Uptime</span>
          <span className="job-tile-value tnum">{fmtUptime(h?.uptime)}</span>
          <span className="job-tile-sub">{RANGES.find(r => r.value === range)?.long} · 30 days {fmtUptime(c.uptime_30d)}</span>
        </div>
        {c.kind !== 'push' && <div className="job-tile">
          <span className="job-tile-label"><Icon name="zap" size={13} />Response</span>
          <span className="job-tile-value tnum">{fmtMs(h?.avg_ms)}</span>
          <span className="job-tile-sub">average · newest {fmtMs(c.last_ms)}</span>
        </div>}
        {days !== null && <div className={`job-tile ${days < 14 ? 'warn' : ''}`}>
          <span className="job-tile-label"><Icon name="lock" size={13} />Certificate</span>
          <span className="job-tile-value tnum">{days < 0 ? 'Expired' : `${days} days`}</span>
          <span className="job-tile-sub">expires {new Date(c.cert_expires * 1000).toLocaleDateString()}</span>
        </div>}
        {c.kind === 'push' && <div className="job-tile">
          <span className="job-tile-label"><Icon name="clock" size={13} />Last push</span>
          <span className="job-tile-value">{c.last_at ? ago(c.last_at) : 'never'}</span>
          <span className="job-tile-sub">{c.next_at ? `missing after ${fmtDateTime(c.next_at, h12)}` : ''}</span>
        </div>}
      </div>
      {c.last_msg && <p className="small up-last"><span className="muted">Newest result{c.last_at ? ` (${ago(c.last_at)})` : ''}:</span> <span className="mono">{c.last_msg}</span></p>}
      {pushURL && <div className="stack tight">
        <Field label="Push URL" help={<>Call it when the job succeeds, e.g. <span className="mono">curl -fsS -m 10 "{pushURL}?status=up&msg=OK&ping="</span>. Add <span className="mono">status=down</span> to report a failure. Uptime Kuma’s push URLs work unchanged.</>} group>
          <CopyField value={pushURL} label="Push URL" />
        </Field>
      </div>}
      <div className="job-bar">
        <Segmented size="sm" label="Range" value={range} onChange={setRange} options={RANGES.map(r => ({ value: r.value, label: r.label, title: r.long }))} />
        <span className="spacer" />
        {onRun && <button type="button" className="btn sm" onClick={onRun}><Icon name="refresh" size={13} />Check now</button>}
        {onEdit && <button type="button" className="btn sm" onClick={onEdit}><Icon name="edit" size={13} />Edit</button>}
      </div>
      {c.kind !== 'push' && (
        <section className="chart-card">
          <header className="chart-head"><Icon name="zap" size={15} className="muted" /><h3>Response time</h3><span className="spacer" />{h && h.points.some(p => p.down > 0) && <span className="chart-now up-bad">{plural(h.points.reduce((a, p) => a + p.down, 0), 'failed result')}</span>}</header>
          <LineChart xs={xs} series={[{ key: 'ms', label: 'Response', values: (h?.points || []).map(p => p.ms) }]} step={h?.step_s || 300} h12={h12} loading={hist.loading} height={170} format={fmtMs} tickFormat={v => `${v}`} label={`Response time of ${c.name}`} />
        </section>
      )}
      {h && h.points.length > 0 && <DownStrip points={h.points} />}
      <div>
        <h4 className="inet-h">Ups and downs</h4>
        {!h?.events.length ? <p className="muted small">{hist.loading ? 'Loading…' : 'It hasn’t gone down in the time kept (13 months).'}</p> : (
          <ul className="up-events">
            {h.events.map((e, i) => {
              const back = h.events[i - 1]
              return (
                <li key={e.t}>
                  <span className={`up-dot ${e.s === 0 ? 'down' : 'up'}`} />
                  <span className="nowrap small">{fmtDateTime(e.t, h12)}</span>
                  <span className="small strong">{e.s === 0 ? 'Down' : 'Up'}</span>
                  <span className="muted small ellipsis grow" title={e.msg}>{e.msg}</span>
                  {e.s === 0 && back && <span className="muted xsmall nowrap">for {fmtSpan(back.t - e.t)}</span>}
                </li>
              )
            })}
          </ul>
        )}
      </div>
      {c.notes && <p className="small muted">{c.notes}</p>}
    </div>
  )
}

function fmtSpan(s: number): string {
  if (s < 90) return `${s} s`
  if (s < 5400) return `${Math.round(s / 60)} min`
  if (s < 172800) return `${(s / 3600).toFixed(1)} h`
  return `${Math.round(s / 86400)} days`
}

/** Each bucket of the range as a block: green, partly failed, or failed. */
function DownStrip({ points }: { points: CheckHistory['points'] }) {
  return (
    <div className="up-strip" aria-label="Results over the range">
      {points.map(p => {
        const all = p.up + p.down
        const cls = !all ? 'bb-none' : !p.down ? 'bb-up' : p.down >= all ? 'bb-down' : 'bb-pend'
        return <i key={p.t} className={cls} title={`${new Date(p.t * 1000).toLocaleString()} · ${all ? `${fmtUptime(p.up / all)} up (${p.down} failed of ${all})` : 'no results'}`} />
      })}
    </div>
  )
}

// ---------- add / edit ----------

function CheckDialog({ server, check, groups, onClose, onSaved }: { server: Server; check: Partial<Check>; groups: string[]; onClose: () => void; onSaved: (c: Check) => void }) {
  const { toast } = useToast()
  const mobile = useIsMobile()
  const isNew = !check.id
  const [f, setF] = useState<Partial<Check>>({
    kind: 'http', name: '', target: '', port: 0, method: 'GET', keyword: '', invert_keyword: false, status_codes: '200-299', ignore_tls: false, follow_redirects: true,
    dns_server: '', dns_type: 'A', interval_s: 60, retry_s: 60, timeout_s: 30, retries: 1, enabled: true, notify: true, public: false, group: '', notes: '', ...check,
  })
  const [busy, setBusy] = useState(false)
  const set = (p: Partial<Check>) => setF(x => ({ ...x, ...p }))
  const kind = KINDS.find(k => k.value === f.kind)!
  const every = f.kind === 'push' ? PUSH_EVERY : EVERY
  const everyOpts = every.some(o => o.value === f.interval_s) ? every : [...every, { value: f.interval_s!, label: `Every ${f.interval_s} s` }]

  // a website check named after its host when no name was typed
  const autoName = () => {
    if (f.name || f.kind !== 'http') return
    try { const h = new URL(f.target || '').hostname; if (h) set({ name: h.split('.')[0].replace(/^./, x => x.toUpperCase()) }) } catch { /* not a URL yet */ }
  }
  const save = async () => {
    setBusy(true)
    try {
      const body = { ...f, port: Number(f.port) || 0 }
      const c = isNew ? await api.post<Check>(`${V1}/servers/${server.id}/checks`, body) : await api.put<Check>(`${V1}/servers/${server.id}/checks/${check.id}`, body)
      toast(isNew ? `${c.name} added${c.kind === 'push' ? ' — copy its push URL' : ''}` : 'Saved')
      onSaved(c)
    } catch (e) { toast(errMsg(e), 'err') } finally { setBusy(false) }
  }
  return (
    <Modal title={isNew ? 'Add a check' : `Edit ${check.name}`} size="lg" onClose={onClose} footer={<>
      <button className="btn" onClick={onClose}>Cancel</button>
      <button className="btn primary" disabled={busy} onClick={save}>{isNew ? 'Add check' : 'Save'}</button>
    </>}>
      <form className="stack" onSubmit={e => { e.preventDefault(); save() }}>
        <Field label="Type" group help={kind.help}>
          {mobile ? <Select value={f.kind!} onChange={v => set({ kind: v })} options={KINDS.map(k => ({ value: k.value, label: k.label, icon: <Icon name={k.icon} size={14} /> }))} label="Type" />
            : <Segmented label="Type" value={f.kind!} onChange={v => set({ kind: v })} options={KINDS.map(k => ({ value: k.value, label: <span className="row"><Icon name={k.icon} size={13} />{k.label}</span> }))} />}
        </Field>
        {f.kind === 'http' && <Field label="URL"><input value={f.target} onChange={e => set({ target: e.target.value })} onBlur={autoName} placeholder="https://jellyfin.example.com/health" inputMode="url" autoFocus={isNew} /></Field>}
        {f.kind === 'tcp' && <div className="two-col">
          <Field label="Host"><input value={f.target} onChange={e => set({ target: e.target.value })} placeholder="192.168.1.10 or nas.local" autoFocus={isNew} /></Field>
          <Field label="Port"><input value={f.port || ''} onChange={e => set({ port: parseInt(e.target.value) || 0 })} inputMode="numeric" placeholder="22" /></Field>
        </div>}
        {f.kind === 'dns' && <>
          <div className="two-col">
            <Field label="Name to look up"><input value={f.target} onChange={e => set({ target: e.target.value })} placeholder="example.com" autoFocus={isNew} /></Field>
            <Field label="Record type" group><Select value={f.dns_type || 'A'} onChange={v => set({ dns_type: v })} options={DNS_TYPES} label="Record type" /></Field>
          </div>
          <div className="two-col">
            <Field label="DNS server" help="Empty = the one this machine uses. Set it to test your own resolver, e.g. AdGuard."><input value={f.dns_server} onChange={e => set({ dns_server: e.target.value })} placeholder="127.0.0.1" /></Field>
            <Field label="Port"><input value={f.port || ''} onChange={e => set({ port: parseInt(e.target.value) || 0 })} inputMode="numeric" placeholder="53" /></Field>
          </div>
        </>}
        <Field label="Name"><input value={f.name} onChange={e => set({ name: e.target.value })} placeholder={f.kind === 'push' ? 'Nightly backup' : 'Jellyfin'} autoFocus={isNew && f.kind === 'push'} /></Field>
        {f.kind === 'http' && <div className="two-col">
          <Field label="The page must" group>
            <Select value={f.keyword ? (f.invert_keyword ? 'without' : 'with') : 'any'} onChange={v => set({ invert_keyword: v === 'without', keyword: v === 'any' ? '' : f.keyword || '' })}
              options={[{ value: 'any', label: 'Just answer with a good status' }, { value: 'with', label: 'Contain a word' }, { value: 'without', label: 'Not contain a word' }]} label="Keyword" />
          </Field>
          {(f.keyword !== '' || f.invert_keyword) ? <Field label="Word" help="Case-sensitive, anywhere in the response."><input value={f.keyword} onChange={e => set({ keyword: e.target.value })} placeholder="Healthy" /></Field> : <span />}
        </div>}
        <div className="two-col">
          <Field label={f.kind === 'push' ? 'Expect a push at least every' : 'Check'} group><Select value={f.interval_s!} onChange={v => set({ interval_s: v, timeout_s: Math.min(f.timeout_s || 30, Math.max(1, Math.floor(v * 0.8))) })} options={everyOpts} label="Interval" /></Field>
          <Field label="Counts as down" group help="Failures in between show as “retrying”, checked again sooner."><Select value={f.retries!} onChange={v => set({ retries: v })} options={RETRIES.some(o => o.value === f.retries) ? RETRIES : [...RETRIES, { value: f.retries!, label: `After ${f.retries! + 1} failures` }]} label="Retries" /></Field>
        </div>
        <div className="two-col">
          <Field label="Group" help="Checks are listed by group, here and on the status page."><input value={f.group} onChange={e => set({ group: e.target.value })} list="up-groups" placeholder="Media, Apps, Network…" /></Field>
          <datalist id="up-groups">{groups.map(g => <option key={g} value={g} />)}</datalist>
          <div className="stack tight up-switches">
            <label className="logs-check"><Switch checked={!!f.notify} onChange={v => set({ notify: v })} label="Send alerts" /><span>Send alerts</span></label>
            <label className="logs-check"><Switch checked={!!f.public} onChange={v => set({ public: v })} label="On the status page" /><span>On the status page</span></label>
          </div>
        </div>
        <details className="quiet-note">
          <summary>More options</summary>
          <div className="stack">
            <div className="three-col">
              <Field label="Retry after" help="seconds, while failing"><input value={f.retry_s} onChange={e => set({ retry_s: parseInt(e.target.value) || 0 })} inputMode="numeric" /></Field>
              {f.kind !== 'push' && <Field label="Timeout" help="seconds"><input value={f.timeout_s} onChange={e => set({ timeout_s: parseInt(e.target.value) || 0 })} inputMode="numeric" /></Field>}
              {f.kind === 'http' && <Field label="Good status codes" help="e.g. 200-299, 301, 4xx"><input value={f.status_codes} onChange={e => set({ status_codes: e.target.value })} /></Field>}
            </div>
            {f.kind === 'http' && <div className="two-col">
              <Field label="Method" group><Select value={f.method || 'GET'} onChange={v => set({ method: v })} options={['GET', 'HEAD', 'POST', 'OPTIONS'].map(m => ({ value: m, label: m }))} label="Method" /></Field>
              <div className="stack tight up-switches">
                <label className="logs-check"><Switch checked={!!f.follow_redirects} onChange={v => set({ follow_redirects: v })} label="Follow redirects" /><span>Follow redirects</span></label>
                <label className="logs-check"><Switch checked={!!f.ignore_tls} onChange={v => set({ ignore_tls: v })} label="Ignore certificate errors" /><span>Ignore certificate errors</span></label>
              </div>
            </div>}
            <Field label="Notes"><textarea rows={2} value={f.notes} onChange={e => set({ notes: e.target.value })} placeholder="What it is, what to do when it’s down" /></Field>
          </div>
        </details>
        {!isNew && <label className="logs-check"><Switch checked={!!f.enabled} onChange={v => set({ enabled: v })} label="Checking" /><span>{f.enabled ? 'Checking' : 'Paused'}</span></label>}
        <button type="submit" hidden />
      </form>
    </Modal>
  )
}

// ---------- status page ----------

function StatusPageDialog({ server, page, checks, onClose, onSaved }: { server: Server; page: StatusPage | null; checks: Check[]; onClose: () => void; onSaved: () => void }) {
  const { toast } = useToast()
  const [f, setF] = useState<StatusPage>(page || { server_id: server.id, slug: 'home', title: server.name, description: '', enabled: true })
  const [pub, setPub] = useState<Record<number, boolean>>(Object.fromEntries(checks.map(c => [c.id, c.public])))
  const [busy, setBusy] = useState(false)
  const save = async () => {
    setBusy(true)
    try {
      await api.put(`${V1}/servers/${server.id}/status-page`, f)
      for (const c of checks) if (!!pub[c.id] !== c.public) await api.put(`${V1}/servers/${server.id}/checks/${c.id}`, { public: !!pub[c.id] })
      toast('Saved')
      onSaved()
    } catch (e) { toast(errMsg(e), 'err') } finally { setBusy(false) }
  }
  const origin = window.location.origin
  return (
    <Modal title="Status page" size="lg" onClose={onClose} footer={<>
      <button className="btn" onClick={onClose}>Cancel</button>
      <button className="btn primary" disabled={busy} onClick={save}>Save</button>
    </>}>
      <div className="stack">
        <label className="logs-check"><Switch checked={f.enabled} onChange={v => setF({ ...f, enabled: v })} label="Published" /><span><b>Published</b><br /><span className="muted xsmall">Anyone who can reach this app can open it without signing in. It shows only the checks you pick below: names, up or down, and uptime — never URLs or messages.</span></span></label>
        <div className="two-col">
          <Field label="Address" help={`${origin}/status/${f.slug || '…'}`}><input value={f.slug} onChange={e => setF({ ...f, slug: e.target.value.toLowerCase().replace(/[^a-z0-9-]/g, '-') })} /></Field>
          <Field label="Title"><input value={f.title} onChange={e => setF({ ...f, title: e.target.value })} /></Field>
        </div>
        <Field label="Description"><input value={f.description} onChange={e => setF({ ...f, description: e.target.value })} placeholder="Optional" /></Field>
        <p className="muted xsmall">Apps that read Uptime Kuma’s status page (Homepage’s <span className="mono">uptimekuma</span> widget, Raycast) can point at this app with slug <span className="mono">{f.slug}</span>.</p>
        <Field label={`On the page (${Object.values(pub).filter(Boolean).length})`} group>
          <div className="up-pick">
            {checks.map(c => <label key={c.id} className="logs-check"><input type="checkbox" checked={!!pub[c.id]} onChange={e => setPub({ ...pub, [c.id]: e.target.checked })} /><span>{c.name}{c.group && <span className="muted xsmall"> · {c.group}</span>}</span></label>)}
          </div>
        </Field>
      </div>
    </Modal>
  )
}

// ---------- import ----------

function KumaImportDialog({ server, onClose, onDone }: { server: Server; onClose: () => void; onDone: () => void }) {
  const { toast } = useToast()
  const [file, setFile] = useState<File | null>(null)
  const [busy, setBusy] = useState(false)
  const [res, setRes] = useState<{ added: number; updated: number; skipped: string[] } | null>(null)
  const go = async () => {
    if (!file) return
    setBusy(true)
    try {
      const r = await upload<{ added: number; updated: number; skipped: string[] }>(`${V1}/servers/${server.id}/checks/import`, 'file', file)
      setRes(r)
      onDone()
    } catch (e) { toast(errMsg(e), 'err') } finally { setBusy(false) }
  }
  return (
    <Modal title="Import from Uptime Kuma" onClose={onClose} footer={res ? <button className="btn primary" onClick={onClose}>Done</button> : <>
      <button className="btn" onClick={onClose}>Cancel</button>
      <button className="btn primary" disabled={!file || busy} onClick={go}>{busy ? 'Importing…' : 'Import'}</button>
    </>}>
      {res ? (
        <div className="stack">
          <div className="callout success"><Icon name="check-circle" size={15} /><span>Added {plural(res.added, 'check')}{res.updated ? `, updated ${res.updated}` : ''}. Push URLs keep their tokens, so scripts only need the new address.</span></div>
          {res.skipped.length > 0 && <div className="callout warn"><Icon name="alert" size={15} /><span>Skipped: {res.skipped.join('; ')}</span></div>}
        </div>
      ) : (
        <div className="stack">
          <p className="small">In Uptime Kuma open <b>Settings → Backup → Export</b> and pick the JSON file here. HTTP, keyword, port, DNS and push monitors come over with their intervals, retries and push tokens. Importing again updates the checks it made before.</p>
          <input type="file" accept=".json,application/json" onChange={e => setFile(e.target.files?.[0] || null)} />
        </div>
      )}
    </Modal>
  )
}

// ---------- on the overview ----------

export function UptimeStrip({ server, onOpen }: { server: Server; onOpen: () => void }) {
  const { version } = useApp()
  const info = useAsync(() => api.get<ChecksInfo>(`${V1}/servers/${server.id}/checks`).catch(() => null), [server.id, version])
  const list = (info.data?.checks || []).filter(c => c.enabled)
  if (!list.length) return null
  const down = list.filter(c => c.status === 0)
  const up = list.filter(c => c.status === 1).length
  return (
    <button type="button" className={`inet-strip ${down.length ? 'bad' : ''}`} onClick={onOpen}>
      <Icon name="globe" size={14} className="muted" />
      <span className="strong">Uptime</span>
      <span className="tnum">{up} of {list.length} up</span>
      {down.length > 0 && <span className="inet-bad ellipsis"><Icon name="x-circle" size={12} />{down.map(c => c.name).join(', ')} down</span>}
      <span className="spacer" />
      <BeatSummary beats={list.flatMap(c => c.beats)} />
      <Icon name="chevron-right" size={14} className="muted" />
    </button>
  )
}

/** All checks' last hour as one bar: worst result per minute. */
function BeatSummary({ beats }: { beats: Beat[] }) {
  const now = Math.floor(Date.now() / 1000)
  const slots: number[] = Array(30).fill(-1)
  for (const b of beats) {
    const i = 29 - Math.floor((now - b.t) / 120)
    if (i >= 0 && i < 30) slots[i] = slots[i] === -1 ? b.s : (b.s === 0 || slots[i] === 0 ? 0 : b.s === 2 || slots[i] === 2 ? 2 : 1)
  }
  return <span className="hide-sm"><BeatBar beats={slots.map((s, i) => ({ s: s === -1 ? 9 : s, t: i }))} n={30} /></span>
}
