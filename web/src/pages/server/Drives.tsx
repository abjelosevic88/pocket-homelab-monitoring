// Drives: the SMART health of every disk a server reports. Cards show where
// each drive stands (failing, worth watching, fine, gone); a card opens the
// drive: what's wrong, its facts, temperature / error / wear history, every
// attribute (a row charts its raw value) and the self-test log.
import { type ReactNode, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { api, V1 } from '../../lib/api'
import { useApp } from '../../lib/app-context'
import { ago, fmtBytes, fmtDateTime, fmtNum, fmtTemp, plural, toUnit } from '../../lib/format'
import type { Drive, DrivePoint, DriveView, Server } from '../../lib/types'
import { LineChart, type Series } from '../../components/Chart'
import { Icon } from '../../components/icons'
import { Confirm, Empty, errMsg, Field, Loading, MenuButton, Modal, Segmented, useAsync, useToast, type MenuItem } from '../../components/ui'
import { Panel } from './Panels'

type Range = '7' | '30' | '90' | '365' | 'all'
const RANGES: { value: Range; label: string }[] = [{ value: '7', label: '7d' }, { value: '30', label: '30d' }, { value: '90', label: '90d' }, { value: '365', label: '1y' }, { value: 'all', label: 'All' }]

// ---------- small bits ----------

/** failed | warning | passed | missing | asleep: what a card leads with. */
export function driveState(v: DriveView): 'failed' | 'missing' | 'warning' | 'passed' {
  if (v.drive.status === 'failed') return 'failed'
  if (v.missing) return 'missing'
  return v.drive.status === 'warning' ? 'warning' : 'passed'
}
const STATE_RANK = { failed: 0, missing: 1, warning: 2, passed: 3 }
export const sortDrives = (list: DriveView[]) => [...list].sort((a, b) => STATE_RANK[driveState(a)] - STATE_RANK[driveState(b)] || a.drive.name.localeCompare(b.drive.name, undefined, { numeric: true }))

export function DriveBadge({ v, sm }: { v: DriveView; sm?: boolean }) {
  const s = driveState(v)
  const cls = `lvl-badge ${sm ? 'sm' : ''} ${s === 'failed' ? 'crit' : s === 'passed' ? 'ok' : s === 'missing' ? 'crit' : 'warn'}`
  const word = { failed: 'Failing', missing: 'Missing', warning: 'Watch', passed: 'Healthy' }[s]
  return <span className={cls}><Icon name={s === 'passed' ? 'check-circle' : 'alert'} size={sm ? 11 : 12} />{word}</span>
}

export function fmtAge(hours: number | null | undefined): string {
  if (hours === null || hours === undefined || hours <= 0) return '–'
  const d = hours / 24
  if (d < 2) return `${hours} h`
  if (d < 365) return `${Math.round(d)} days`
  return `${(d / 365.25).toFixed(1)} years`
}

/** Capacity in decimal units, as the label on the drive says (12 TB, 512 GB). */
export function fmtCap(n: number): string {
  if (!n) return '–'
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  let v = n, i = 0
  while (v >= 1000 && i < u.length - 1) { v /= 1000; i++ }
  return `${v >= 100 ? v.toFixed(0) : v.toFixed(1).replace(/\.0$/, '')} ${u[i]}`
}

const kindOf = (d: Drive) => d.protocol === 'NVMe' ? 'NVMe SSD' : d.rpm > 0 ? `HDD · ${fmtNum(d.rpm)} rpm` : 'SSD'
const shortDev = (n: string) => n.replace(/^\/dev\//, '')

function why(v: DriveView): string | null {
  const d = v.drive
  if (d.problems?.length) return d.problems[0]
  if (v.missing) return `Not found by the last check — last read ${ago(v.last_seen * 1000)}`
  if (d.warnings?.length) return d.warnings[0]
  return null
}

// ---------- cards ----------

export function DriveCard({ v, onOpen, showServer }: { v: DriveView; onOpen: () => void; showServer?: boolean }) {
  const { settings } = useApp()
  const unit = settings?.temp_unit || 'C'
  const d = v.drive
  const s = driveState(v)
  const reason = why(v)
  const hot = d.temp_limit > 0 && d.temp > d.temp_limit
  return (
    <button type="button" className={`drive-card ${s}`} onClick={onOpen}>
      <div className="drive-head">
        <span className={`drive-dot ${s}`} />
        <div className="grow min0">
          <div className="drive-title ellipsis">{d.model || shortDev(d.name)}</div>
          <div className="muted xsmall ellipsis">{showServer && <><span className="srv-swatch sm" style={{ ['--c' as string]: v.server_color }} />{v.server_name} · </>}{shortDev(d.name)} · {fmtCap(d.capacity)} · {kindOf(d)}</div>
        </div>
        <DriveBadge v={v} sm />
      </div>
      <dl className="drive-stats">
        <div><dt>Temp</dt><dd className={`tnum ${hot ? 'danger-text' : ''}`}>{d.temp > 0 ? fmtTemp(d.temp, unit) : '–'}</dd></div>
        <div><dt>Powered on</dt><dd className="tnum">{fmtAge(d.power_on_hours)}</dd></div>
        {d.wear !== undefined && d.wear !== null
          ? <div><dt>Worn</dt><dd className={`tnum ${d.wear >= 80 ? 'warn-text' : ''}`}>{d.wear}%</dd></div>
          : <div><dt>Bad sectors</dt><dd className={`tnum ${d.errors > 0 ? 'warn-text' : ''}`}>{fmtNum(d.errors)}</dd></div>}
        <div><dt>Written</dt><dd className="tnum">{d.written ? fmtBytes(d.written, 0) : '–'}</dd></div>
      </dl>
      {reason && <div className={`drive-why ${s === 'failed' || s === 'missing' ? 'crit' : 'warn'}`}><Icon name="alert" size={12} /><span className="ellipsis">{reason}</span></div>}
      {!reason && (d.standby || d.error) && <div className="drive-why muted"><Icon name="moon" size={12} /><span className="ellipsis">{d.standby ? 'Asleep at the last check — values from ' + ago(d.read_at) : d.error}</span></div>}
    </button>
  )
}

export function DriveGrid({ list, onOpen, showServer }: { list: DriveView[]; onOpen: (v: DriveView) => void; showServer?: boolean }) {
  return <div className="drive-grid">{sortDrives(list).map(v => <DriveCard key={`${v.server_id}/${v.drive.key}`} v={v} showServer={showServer} onOpen={() => onOpen(v)} />)}</div>
}

/** One line on a server's overview: how its drives are. */
export function DrivesStrip({ server, onOpen }: { server: Server; onOpen: () => void }) {
  const h = server.summary
  if (!h?.drives_total) return null
  const bad = h.drives_failed + h.drives_warning
  return (
    <button type="button" className={`inet-strip ${h.drives_failed ? 'bad' : ''}`} onClick={onOpen}>
      <Icon name="disk" size={14} className="muted" />
      <span className="strong">Drives</span>
      <span className="tnum">{bad ? `${h.drives_total - bad} of ${h.drives_total} healthy` : `${plural(h.drives_total, 'drive')}, all healthy`}</span>
      {h.drives_failed > 0 && <span className="inet-bad ellipsis"><Icon name="x-circle" size={12} />{h.drives_failed} failing</span>}
      {h.drives_warning > 0 && <span className="warn-text small">{h.drives_warning} to watch</span>}
      <span className="spacer" />
      <Icon name="chevron-right" size={14} className="muted" />
    </button>
  )
}

// ---------- a server's tab ----------

export function DrivesView({ server, open, onOpen }: { server: Server; open: string; onOpen: (key: string) => void }) {
  const { version } = useApp()
  const list = useAsync(() => api.get<DriveView[]>(`${V1}/servers/${server.id}/drives`), [server.id, version])
  if (open) return <DriveDetail serverId={server.id} driveKey={open} onBack={() => onOpen('')} />
  if (!list.data) return list.error ? <Empty title="Couldn’t load the drives" icon="alert">{errMsg(list.error)}</Empty> : <Loading />
  if (!list.data.length) return (
    <Empty title="No drive health yet" icon="disk">
      {server.kind === 'glances'
        ? 'Glances doesn’t report SMART. Run the Pocket agent on this machine (privileged) to see its drives.'
        : <>Drive health comes from <span className="mono">smartctl</span>, which needs root and the disks: run the {server.kind === 'local' ? 'app' : 'agent'} container privileged. The first check runs a minute after start, then every 15 minutes.</>}
    </Empty>
  )
  return <DriveGrid list={list.data} onOpen={v => onOpen(v.drive.key)} />
}

// ---------- one drive ----------

export function DriveDetail({ serverId, driveKey, onBack, showServer }: { serverId: number; driveKey: string; onBack: () => void; showServer?: boolean }) {
  const { settings, h12, version, bump } = useApp()
  const { toast } = useToast()
  const unit = settings?.temp_unit || 'C'
  const [range, setRange] = useState<Range>('30')
  const [attr, setAttr] = useState<string>('')
  const [forget, setForget] = useState(false)
  const res = useAsync(() => api.get<{ drive: DriveView; history: DrivePoint[] }>(`${V1}/servers/${serverId}/drives/${encodeURIComponent(driveKey)}?days=${range}`), [serverId, driveKey, range, version])
  const pts = res.data?.history || []
  const xs = useMemo(() => pts.map(p => p.t), [pts])

  if (!res.data) return res.error ? <Empty title="No such drive" icon="disk"><button className="btn mt-s" onClick={onBack}>Back to the drives</button></Empty> : <Loading />
  const v = res.data.drive
  const d = v.drive
  const st = driveState(v)
  const temps: Series[] = [{ key: 't1', label: 'Temperature', values: pts.map(p => p.temp === null ? null : toUnit(p.temp, unit)) }]
  const errs: Series[] = [{ key: 't4', label: 'Bad sectors / media errors', values: pts.map(p => p.errors) }]
  const wear: Series[] = [{ key: 't2', label: 'Endurance used', values: pts.map(p => p.wear) }]
  const hasWear = pts.some(p => p.wear !== null)
  const sel = d.attrs.find(a => a.key === attr)
  const menu: MenuItem[] = [
    { label: 'Copy serial number', icon: 'copy', onSelect: () => { navigator.clipboard?.writeText(d.serial); toast('Copied') } },
    ...(v.role === 'owner' ? ['sep' as const, { label: 'Forget this drive…', icon: 'trash', danger: true, onSelect: () => setForget(true) }] : []),
  ]
  const facts: [string, ReactNode][] = [
    ...(showServer ? [['Server', <Link key="s" to={`/server/${v.server_id}?tab=drives`}>{v.server_name}</Link>] as [string, ReactNode]] : []),
    ['Device', <span key="d" className="mono">{d.name}{d.type ? <span className="muted"> ({d.type})</span> : null}</span>],
    ['Model', <>{d.model}{d.family ? <span className="muted"> · {d.family}</span> : null}</>],
    ['Serial', <span key="sn" className="mono">{d.serial}</span>],
    ...(d.wwn ? [['WWN', <span key="w" className="mono">{d.wwn}</span>] as [string, ReactNode]] : []),
    ['Capacity', `${fmtCap(d.capacity)} · ${kindOf(d)}${d.form_factor ? ` · ${d.form_factor}` : ''}`],
    ...(d.interface ? [['Interface', d.interface] as [string, ReactNode]] : []),
    ...(d.firmware ? [['Firmware', d.firmware] as [string, ReactNode]] : []),
    ['SMART self-check', d.smart_passed === false ? <span className="danger-text strong">Failing</span> : d.smart_passed ? 'Passed' : '–'],
    ['Powered on', `${fmtAge(d.power_on_hours)} (${fmtNum(d.power_on_hours)} h) · ${fmtNum(d.power_cycles)} power cycles${d.unsafe_shutdowns ? ` · ${fmtNum(d.unsafe_shutdowns)} unsafe shutdowns` : ''}`],
    ['Temperature', `${d.temp > 0 ? fmtTemp(d.temp, unit) : '–'}${d.temp_max ? ` · highest ever ${fmtTemp(d.temp_max, unit)}` : ''} · runs hot over ${fmtTemp(d.temp_limit, unit)}`],
    ...(d.wear !== undefined ? [['Endurance used', `${d.wear}%${d.spare !== undefined ? ` · spare ${d.spare}% (fails under ${d.spare_threshold}%)` : ''}`] as [string, ReactNode]] : []),
    ['Bad sectors', d.protocol === 'NVMe' ? `${fmtNum(d.media_errors || 0)} media errors` : `${fmtNum(d.reallocated || 0)} reallocated · ${fmtNum(d.pending || 0)} pending · ${fmtNum(d.uncorrectable || 0)} uncorrectable${d.crc_errors ? ` · ${fmtNum(d.crc_errors)} cable (CRC) errors` : ''}`],
    ...(d.error_log ? [['Error log', `${fmtNum(d.error_log)} entries`] as [string, ReactNode]] : []),
    ...(d.written ? [['Data written', `${fmtBytes(d.written, 1)}${d.read ? ` · read ${fmtBytes(d.read, 1)}` : ''}`] as [string, ReactNode]] : []),
    ['Last read', <span key="lr" title={fmtDateTime(d.read_at, h12)}>{ago(d.read_at)}{d.standby ? ' (asleep since — not woken up for it)' : ''}{d.error ? ` — the last try failed: ${d.error}` : ''}</span>],
    ['Watched since', fmtDateTime(v.first_seen * 1000, h12)],
  ]
  const ata = d.protocol !== 'NVMe'
  return (
    <div className="stack">
      <div className="drive-bar">
        <button className="btn ghost sm" onClick={onBack}><Icon name="chevron-left" size={15} />Drives</button>
        <span className={`drive-dot ${st}`} />
        <h2 className="drive-h ellipsis">{d.model}</h2>
        <span className="muted small mono">{shortDev(d.name)}</span>
        <DriveBadge v={v} />
        <span className="spacer" />
        <MenuButton label="Drive options" align="right" items={menu} width={220}><Icon name="more" size={16} /></MenuButton>
      </div>

      {v.missing && <div className="callout danger"><Icon name="alert" size={15} /><span><b>Not found by the last check.</b> A disk that dies often just disappears. If you took it out on purpose, forget it from the ⋯ menu.</span></div>}
      {!!d.problems?.length && <div className="callout danger"><Icon name="alert" size={15} /><div><b>Failing — back up what's on it and replace it.</b><ul className="drive-list">{d.problems.map(p => <li key={p}>{p}</li>)}</ul></div></div>}
      {!!d.warnings?.length && <div className="callout warn"><Icon name="alert" size={15} /><div><b>Worth keeping an eye on</b><ul className="drive-list">{d.warnings.map(p => <li key={p}>{p}</li>)}</ul></div></div>}

      <div className="panel-grid">
        <Panel title="Drive" icon="disk"><dl className="set-dl">{facts.map(([k, x]) => <div key={k}><dt>{k}</dt><dd>{x}</dd></div>)}</dl></Panel>
        <Panel title="History" icon="activity" actions={<Segmented size="sm" label="Range" value={range} onChange={setRange} options={RANGES} />}>
          <div className="stack">
            <div><div className="chart-title muted small">Temperature</div>
              <LineChart xs={xs} series={temps} h12={h12} step={3600} format={x => `${x.toFixed(0)}°${unit}`} label="Drive temperature over time" area={false} height={140}
                guides={d.temp_limit ? [{ value: toUnit(d.temp_limit, unit), label: 'hot' }] : []} empty="No history yet — a point is kept every hour" /></div>
            <div><div className="chart-title muted small">{ata ? 'Reallocated, pending and uncorrectable sectors' : 'Media errors'}</div>
              <LineChart xs={xs} series={errs} h12={h12} step={3600} format={x => fmtNum(x)} label="Drive errors over time" area={false} height={110} empty="No history yet" /></div>
            {hasWear && <div><div className="chart-title muted small">Endurance used</div>
              <LineChart xs={xs} series={wear} h12={h12} step={3600} yMax={100} format={x => `${x}%`} label="Drive wear over time" area={false} height={110} /></div>}
          </div>
        </Panel>
      </div>

      {sel && (
        <Panel title={`${sel.name} over time`} icon="activity" actions={<button className="btn ghost sm icon-only" aria-label="Close" onClick={() => setAttr('')}><Icon name="x" size={14} /></button>}>
          <LineChart xs={xs} series={[{ key: 't3', label: sel.name, values: pts.map(p => p.attrs?.[sel.key] ?? null) }]} h12={h12} step={3600} format={x => fmtNum(x)} label={`${sel.name} over time`} area={false} height={140} empty="No history for this attribute yet" />
          {sel.note && <p className="muted small mt-s">{sel.note}</p>}
        </Panel>
      )}

      <Panel title={ata ? 'SMART attributes' : 'NVMe health log'} icon="list" flush>
        <div className="tbl-wrap">
          <table className="table dense drive-attrs">
            <thead><tr>
              {ata && <th className="num">ID</th>}<th>Attribute</th>
              {ata && <><th className="num" title="Normalised value: higher is better">Value</th><th className="num">Worst</th><th className="num" title="Failing at or under this">Threshold</th></>}
              <th className="num">Raw</th><th>Status</th>
            </tr></thead>
            <tbody>
              {d.attrs.map(a => (
                <tr key={a.key} className={`clickable ${a.key === attr ? 'selected' : ''}`} onClick={() => setAttr(a.key === attr ? '' : a.key)} title={a.note || 'Show its history'}>
                  {ata && <td className="num tnum muted">{a.id}</td>}
                  <td>{a.name}{a.prefail ? <span className="muted xsmall"> · pre-fail</span> : null}{a.note && <Icon name="info" size={12} className="muted attr-info" />}</td>
                  {ata && <><td className="num tnum">{a.value ?? '–'}</td><td className="num tnum">{a.worst ?? '–'}</td><td className="num tnum muted">{a.thresh || '–'}</td></>}
                  <td className="num tnum">{a.raw_str || fmtNum(a.raw)}</td>
                  <td>{a.status === 'fail' ? <span className="lvl-badge crit sm">{a.failed === 'now' ? 'Failing' : 'Bad'}</span> : a.status === 'warn' ? <span className="lvl-badge warn sm">{a.failed === 'past' ? 'Failed before' : 'Watch'}</span> : <span className="lvl-badge ok sm">OK</span>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Panel>

      <Panel title="Self-tests" icon="check-circle" flush>
        {!d.self_tests?.length ? <p className="muted small" style={{ padding: '10px 14px' }}>No self-tests in the drive's log. TrueNAS and <span className="mono">smartd</span> can run them on a schedule; this app only reads the results.</p> : (
          <div className="tbl-wrap">
            <table className="table dense">
              <thead><tr><th>Test</th><th>Result</th><th className="num">At power-on hour</th><th className="num">Ago</th></tr></thead>
              <tbody>{d.self_tests.slice(0, 20).map((t, i) => (
                <tr key={i}>
                  <td>{t.type}</td>
                  <td>{t.running ? <span className="lvl-badge run sm">Running</span> : t.failed ? <span className="lvl-badge crit sm">{t.result}</span> : t.passed ? <span className="lvl-badge ok sm"><Icon name="check" size={11} />{t.result}</span> : <span className="muted">{t.result}</span>}</td>
                  <td className="num tnum">{t.running ? '' : fmtNum(t.hours)}</td>
                  <td className="num tnum muted">{t.running || !t.hours ? '' : fmtAge(Math.max(1, d.power_on_hours - t.hours))}</td>
                </tr>
              ))}</tbody>
            </table>
          </div>
        )}
      </Panel>

      {forget && <Confirm title="Forget this drive?" confirmLabel="Forget"
        message={<>Its history goes and its alerts clear. If the drive is still in the machine it comes back with the next check (every 15 minutes) — with no history.</>}
        onCancel={() => setForget(false)}
        onConfirm={async () => { setForget(false); try { await api.del(`${V1}/servers/${serverId}/drives/${encodeURIComponent(driveKey)}`); bump(); onBack() } catch (e) { toast(errMsg(e), 'err') } }} />}
    </div>
  )
}

// ---------- Scrutiny import ----------

interface ImportResult { points: number; drives: { drive: string; server?: string; points: number; matched: boolean }[] }

export function ScrutinyImport({ onClose }: { onClose: () => void }) {
  const { bump } = useApp()
  const { toast } = useToast()
  const [url, setUrl] = useState('')
  const [busy, setBusy] = useState(false)
  const [done, setDone] = useState<ImportResult | null>(null)
  const run = async () => {
    setBusy(true)
    try { setDone(await api.post<ImportResult>(`${V1}/drives/import/scrutiny`, { url })); bump() } catch (e) { toast(errMsg(e), 'err') } finally { setBusy(false) }
  }
  return (
    <Modal title="Import history from Scrutiny" onClose={onClose} footer={done ? <button className="btn primary" onClick={onClose}>Done</button> : <>
      <button className="btn" onClick={onClose}>Cancel</button>
      <button className="btn primary" disabled={busy || !url.trim()} onClick={run}>{busy ? 'Importing…' : 'Import'}</button>
    </>}>
      {!done ? (
        <div className="stack">
          <p className="muted small">Brings each drive's Scrutiny history (temperature, power-on hours and every attribute) over to the same drive here, matched by serial number. Drives have to show up here first — wait for their first check. Points already here are kept.</p>
          <Field label="Scrutiny address" help="The web address Scrutiny answers on, e.g. http://192.168.1.10:8080">
            <input value={url} onChange={e => setUrl(e.target.value)} placeholder="http://scrutiny:8080" autoFocus onKeyDown={e => { if (e.key === 'Enter' && url.trim()) run() }} />
          </Field>
        </div>
      ) : (
        <div className="stack">
          <p><b>{plural(done.points, 'point')}</b> imported.</p>
          <ul className="drive-list">{done.drives.map(x => (
            <li key={x.drive}>{x.matched ? <Icon name="check" size={13} /> : <Icon name="minus" size={13} className="muted" />} {x.drive}
              <span className="muted"> — {x.matched ? `${x.server}: ${plural(x.points, 'point')}` : 'not found here (yet)'}</span></li>
          ))}</ul>
        </div>
      )}
    </Modal>
  )
}
