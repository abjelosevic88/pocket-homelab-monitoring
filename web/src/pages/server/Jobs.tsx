// Scheduled jobs: every systemd timer on the machine with its last run, what
// it wrote, when it runs next and how the week went; run one now, pause it,
// read its log, file it under a group, mute its alerts. The Activity view is
// the feed of runs that did something or failed, across every job.
import { useEffect, useMemo, useState } from 'react'
import { api, qs, V1 } from '../../lib/api'
import { useApp } from '../../lib/app-context'
import { ago, fmtDateTime, plural } from '../../lib/format'
import type { Job, JobDay, JobLogLine, JobRun, JobsList, JobStatus, Server } from '../../lib/types'
import { Icon } from '../../components/icons'
import { Empty, Field, Loading, MenuButton, Modal, Segmented, Switch, Tabs, errMsg, useAsync, useIsMobile, useToast, type MenuItem } from '../../components/ui'
import { Panel } from './Panels'
import { Search } from './Tables'

const STATUS: Record<JobStatus, { label: string; cls: string; icon: string }> = {
  ok: { label: 'OK', cls: 'ok', icon: 'check-circle' },
  running: { label: 'Running', cls: 'run', icon: 'play' },
  paused: { label: 'Paused', cls: 'off', icon: 'pause' },
  failed: { label: 'Failed', cls: 'crit', icon: 'x-circle' },
  stuck: { label: 'Stuck', cls: 'crit', icon: 'clock' },
  overdue: { label: 'Overdue', cls: 'warn', icon: 'clock' },
  unscheduled: { label: 'Not scheduled', cls: 'warn', icon: 'alert' },
}
const PROBLEM: JobStatus[] = ['failed', 'stuck', 'overdue', 'unscheduled']
type Filter = 'all' | 'problems' | 'running' | 'paused'
type View = 'jobs' | 'activity'

/** "OnCalendar=*-*-* 04:15:00" → "daily at 04:15"; "OnUnitActiveUSec=10min" → "every 10 min". */
export function scheduleText(list: string[] | null | undefined): string {
  if (!list?.length) return 'only when started'
  const parts: string[] = []
  const boot = list.filter(s => /^On(Boot|Startup)/.test(s))
  for (const s of list) {
    const [k, v = ''] = s.split(/=(.*)/)
    if (k === 'OnCalendar') parts.push(calendarText(v))
    else if (k === 'OnUnitActiveUSec' || k === 'OnUnitActiveSec') parts.push(`every ${spanText(v)}`)
    else if (k === 'OnUnitInactiveUSec' || k === 'OnUnitInactiveSec') parts.push(`${spanText(v)} after each run`)
    else if (k === 'OnActiveUSec' || k === 'OnActiveSec') parts.push(`${spanText(v)} after the timer starts`)
  }
  if (!parts.length && boot.length) parts.push(`once, ${spanText(boot[0].split('=')[1] || '')} after boot`)
  return parts.join(' · ') || list.join(', ')
}

function spanText(v: string) {
  return v.replace(/(\d+)min/g, '$1 min').replace(/(\d+)s\b/g, '$1 s').replace(/(\d+)h\b/g, '$1 h').replace(/(\d+)d\b/g, '$1 days').replace(/^1 days$/, 'day').replace(/^1 h$/, 'hour').trim()
}

function calendarText(v: string) {
  const m = v.trim().match(/^(?:([A-Za-z,.]+)\s+)?(\S+)\s+(\S+)$/)
  if (!m) return v
  const [, days, date, time] = m
  const [h, mi, se] = time.split(':')
  const step = (x: string | undefined) => x?.match(/^(?:\*|\d+)\/(\d+)$/)?.[1]
  let when = ''
  if (h === '*' && mi === '*') when = 'every minute'
  else if (h === '*' && step(mi)) when = `every ${step(mi)} min`
  else if (h === '*' && /^\d+$/.test(mi)) when = `hourly at :${mi.padStart(2, '0')}`
  else if (step(h) && /^\d+$/.test(mi)) when = `every ${step(h)} h`
  else if (/^\d+$/.test(h) && /^\d+$/.test(mi)) when = `at ${h.padStart(2, '0')}:${mi.padStart(2, '0')}${se && se !== '00' && se !== '0' ? ':' + se : ''}`
  else return v
  if (date !== '*-*-*') return `${v}`
  if (days) return `${days.replace(/,/g, ', ')} ${when}`
  return when.startsWith('at ') ? `daily ${when}` : when
}

function until(ms: number): string {
  if (!ms) return '—'
  const s = Math.round((ms - Date.now()) / 1000)
  if (s < -60) return `${ago(ms / 1000)} (late)`
  if (s < 45) return 'now'
  if (s < 3600) return `in ${Math.round(s / 60)} min`
  if (s < 86400) return `in ${Math.round(s / 3600)} h`
  const d = Math.round(s / 86400)
  return d === 1 ? 'tomorrow' : `in ${d} days`
}

export function durText(ms: number | null | undefined): string {
  if (ms === null || ms === undefined || ms < 0) return '—'
  if (ms < 1000) return `${ms} ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(ms < 10_000 ? 1 : 0)} s`
  if (ms < 3_600_000) return `${Math.floor(ms / 60_000)} min ${Math.round((ms % 60_000) / 1000)} s`
  return `${Math.floor(ms / 3_600_000)} h ${Math.round((ms % 3_600_000) / 60_000)} min`
}

function StatusBadge({ s, sm }: { s: JobStatus; sm?: boolean }) {
  const st = STATUS[s] || STATUS.ok
  return <span className={`lvl-badge ${sm ? 'sm' : ''} ${st.cls}`}><Icon name={st.icon} size={11} />{st.label}</span>
}

export function JobsView({ server }: { server: Server }) {
  const { version } = useApp()
  const mobile = useIsMobile()
  const { toast } = useToast()
  const [tick, setTick] = useState(0)
  const list = useAsync(() => api.get<JobsList>(`${V1}/servers/${server.id}/jobs`), [server.id, version, tick])
  const days = useAsync(() => api.get<JobDay[]>(`${V1}/servers/${server.id}/jobs/days?days=14`), [server.id, version])
  const [q, setQ] = useState('')
  const [filter, setFilter] = useState<Filter>('all')
  const [view, setView] = useState<View>('jobs')
  const [open, setOpen] = useState<string | null>(null)

  // Countdowns move, and the list is read again every 30 s.
  useEffect(() => { const t = setInterval(() => { if (!document.hidden) setTick(x => x + 1) }, 30_000); return () => clearInterval(t) }, [])

  const data = list.data
  const jobs = data?.jobs || []
  const owner = !!data?.owner
  const counts = useMemo(() => ({
    problems: jobs.filter(j => PROBLEM.includes(j.status) && !j.muted).length,
    running: jobs.filter(j => j.status === 'running').length,
    paused: jobs.filter(j => j.status === 'paused').length,
  }), [jobs])
  const needle = q.trim().toLowerCase()
  const shown = jobs.filter(j => (filter === 'all' || (filter === 'problems' ? PROBLEM.includes(j.status) : j.status === filter))
    && (!needle || `${j.name} ${j.unit} ${j.description} ${j.group} ${j.last_kept?.summary || ''}`.toLowerCase().includes(needle)))
  const groups = useMemo(() => {
    const m = new Map<string, Job[]>()
    for (const j of shown) m.set(j.group, [...(m.get(j.group) || []), j])
    return [...m.entries()].sort((a, b) => a[0].localeCompare(b[0]))
  }, [shown])

  const act = async (j: Job, action: 'run' | 'pause' | 'resume') => {
    try {
      await api.post(`${V1}/servers/${server.id}/jobs/action`, { unit: j.unit, action })
      toast(action === 'run' ? `Started ${j.name}` : action === 'pause' ? `Paused ${j.name}` : `Resumed ${j.name}`)
      list.reload()
      setTimeout(() => list.reload(), 3000)
    } catch (e) { toast(errMsg(e), 'err') }
  }
  const save = async (j: Job, patch: Partial<Pick<Job, 'label' | 'group' | 'notes' | 'muted'>>) => {
    try {
      await api.put(`${V1}/servers/${server.id}/jobs/settings`, { unit: j.unit, ...patch })
      list.reload()
      return true
    } catch (e) { toast(errMsg(e), 'err'); return false }
  }

  if (server.kind === 'glances') return <Empty title="Jobs come through the agent" icon="clock">Glances doesn’t report scheduled jobs. Run the Pocket Monitoring agent on this machine to see its timers here.</Empty>
  if (!data) return list.loading ? <Loading /> : <Empty title="Couldn’t read the jobs" icon="alert">{list.error}</Empty>
  if (!data.available) {
    return (
      <Empty title={data.reason ? 'Scheduled jobs aren’t set up here' : 'Couldn’t read the jobs'} icon="clock">
        {data.reason || data.error || 'The machine didn’t answer.'}
      </Empty>
    )
  }

  const openJob = jobs.find(j => j.unit === open)
  const today = days.data?.[days.data.length - 1]

  return (
    <div className="stack">
      {data.error && <div className="callout warn"><Icon name="alert" size={15} /><span><b>Stale</b> — couldn’t read the jobs {ago(data.checked_at)}: {data.error}</span></div>}
      <div className="job-tiles">
        <JobTile label="Jobs" value={jobs.length} sub={`${plural(counts.running, 'running', 'running')} now`} icon="clock" />
        <JobTile label="Need attention" value={counts.problems} sub={counts.problems ? 'failed, stuck or not running' : 'all fine'} icon={counts.problems ? 'alert' : 'check-circle'} lvl={counts.problems ? 'crit' : 'ok'} onClick={() => { setView('jobs'); setFilter(counts.problems ? 'problems' : 'all') }} />
        <JobTile label="Runs today" value={today?.runs ?? 0} sub={today?.failed ? `${today.failed} failed` : 'none failed'} icon="activity" lvl={today?.failed ? 'warn' : undefined} />
        <JobTile label="Did something today" value={today?.busy ?? 0} sub="runs that wrote output" icon="zap" onClick={() => setView('activity')} />
        {days.data && days.data.length > 0 && <DaysChart days={days.data} />}
      </div>

      <div className="job-bar">
        <Segmented size="sm" label="View" value={view} onChange={setView} options={[{ value: 'jobs', label: 'Jobs' }, { value: 'activity', label: 'Activity' }]} />
        {view === 'jobs' && <>
          <Search value={q} onChange={setQ} placeholder="Find a job" />
          <div className="chips">
            {([['all', `All ${jobs.length}`], ['problems', `Problems ${counts.problems}`], ['running', `Running ${counts.running}`], ['paused', `Paused ${counts.paused}`]] as [Filter, string][]).map(([k, l]) => (
              <button key={k} type="button" className={`chip ${filter === k ? 'active' : ''}`} onClick={() => setFilter(k)}>{l}</button>
            ))}
          </div>
        </>}
        <span className="spacer" />
        <span className="muted xsmall">Read {ago(data.checked_at)}{data.host ? ` from ${data.host}` : ''}</span>
      </div>

      {view === 'activity' ? <Activity server={server} jobs={jobs} onOpen={setOpen} /> : !shown.length ? (
        <Empty title={needle || filter !== 'all' ? 'No job matches' : 'No scheduled jobs'} icon="clock">{!jobs.length ? 'This machine has no systemd timers the reader can see.' : ''}</Empty>
      ) : groups.map(([g, list]) => (
        <Panel key={g} title={g} icon="layers" flush actions={<span className="muted small">{plural(list.length, 'job')}</span>}>
          <div className="tbl-wrap">
            <table className="table dense job-table">
              <thead><tr>
                <th>Job</th>
                <th className="hide-sm">Schedule</th>
                <th>Last run</th>
                <th className="hide-md">Last output</th>
                <th className="hide-sm">Next</th>
                <th className="right hide-md" title="Runs in the last 7 days (failed)">7 days</th>
                <th aria-label="Actions" />
              </tr></thead>
              <tbody>
                {list.map(j => <JobRow key={j.unit} j={j} owner={owner} mobile={mobile} onOpen={() => setOpen(j.unit)} onAct={a => act(j, a)} onMute={m => save(j, { muted: m })} />)}
              </tbody>
            </table>
          </div>
        </Panel>
      ))}

      {openJob && <JobDetail server={server} job={openJob} owner={owner} groups={data.groups} onClose={() => setOpen(null)} onAct={a => act(openJob, a)} onSave={p => save(openJob, p)} />}
    </div>
  )
}

function JobTile({ label, value, sub, icon, lvl, onClick }: { label: string; value: React.ReactNode; sub: string; icon: string; lvl?: 'ok' | 'warn' | 'crit'; onClick?: () => void }) {
  const Tag = onClick ? 'button' : 'div'
  return (
    <Tag type={onClick ? 'button' : undefined} className={`job-tile ${lvl || ''} ${onClick ? 'click' : ''}`} onClick={onClick}>
      <span className="job-tile-label"><Icon name={icon} size={13} />{label}</span>
      <span className="job-tile-value tnum">{value}</span>
      <span className="job-tile-sub">{sub}</span>
    </Tag>
  )
}

function DaysChart({ days }: { days: JobDay[] }) {
  const max = Math.max(1, ...days.map(d => d.busy + d.failed))
  return (
    <div className="job-tile job-days" title="Runs that did something (and failures) per day, last 14 days">
      <span className="job-tile-label"><Icon name="trending-up" size={13} />Last 14 days</span>
      <div className="job-days-bars" role="img" aria-label="Runs that did something per day">
        {days.map(d => (
          <span key={d.day} className="job-day" title={`${d.day}: ${d.runs.toLocaleString()} runs, ${d.busy} did something, ${d.failed} failed`}>
            <span className="bar busy" style={{ height: `${(d.busy / max) * 100}%` }} />
            {d.failed > 0 && <span className="bar fail" style={{ height: `${Math.max(8, (d.failed / max) * 100)}%` }} />}
          </span>
        ))}
      </div>
    </div>
  )
}

function jobMenu(j: Job, owner: boolean, onOpen: () => void, onAct: (a: 'run' | 'pause' | 'resume') => void, onMute: (m: boolean) => void): MenuItem[] {
  const items: MenuItem[] = [{ label: 'Details & runs', icon: 'list', onSelect: onOpen }]
  if (!owner) return items
  if (j.app) {
    if (j.control) items.push({ label: 'Run now', icon: 'play', onSelect: () => onAct('run'), disabled: j.status === 'running' })
    if (j.url) items.push({ label: `Settings in ${j.app_name}`, icon: 'external', onSelect: () => window.open(j.url, '_blank', 'noopener') })
  } else if (j.control) {
    items.push({ label: 'Run now', icon: 'play', onSelect: () => onAct('run'), disabled: j.status === 'running' })
    items.push(j.status === 'paused' ? { label: 'Resume schedule', icon: 'repeat', onSelect: () => onAct('resume') } : { label: 'Pause schedule', icon: 'pause', onSelect: () => onAct('pause') })
  } else {
    items.push({ label: 'System job: read only', icon: 'lock', disabled: true })
  }
  items.push('sep', { label: j.muted ? 'Alert when it fails' : 'Don’t alert for this job', icon: j.muted ? 'bell' : 'bell-off', onSelect: () => onMute(!j.muted) })
  return items
}

function JobRow({ j, owner, mobile, onOpen, onAct, onMute }: { j: Job; owner: boolean; mobile: boolean; onOpen: () => void; onAct: (a: 'run' | 'pause' | 'resume') => void; onMute: (m: boolean) => void }) {
  const last = j.last_run
  const out = j.last_kept
  const lastEnd = last?.end || j.last_end
  // the journal's run is ms-exact; systemd's own timestamps are whole seconds
  const dur = last && last.start ? last.end - last.start : j.last_start && j.last_end >= j.last_start ? Math.max(j.last_end - j.last_start, 0) : null
  const durLabel = dur === null ? '' : !(last && last.start) && dur < 1000 ? 'under 1 s' : durText(dur)
  return (
    <tr className={`job-row ${j.status === 'paused' ? 'dim' : ''}`} onClick={onOpen}>
      <td className="job-name-cell">
        <div className="job-name">
          <span className={`job-dot ${STATUS[j.status]?.cls}`} title={STATUS[j.status]?.label} />
          <span className="strong ellipsis">{j.name}</span>
          {j.status !== 'ok' && <StatusBadge s={j.status} sm />}
          {j.muted && <span title="No alerts for this job"><Icon name="bell-off" size={12} className="faint" /></span>}
          {j.scope === 'system' && <span className="tag">system</span>}
          {j.app && <span className="tag" title={`Runs inside ${j.app_name}`}>app</span>}
        </div>
        <div className="muted xsmall ellipsis job-desc" title={j.problem || j.description}>{j.problem || j.description}</div>
        {mobile && <div className="faint xsmall">{scheduleText(j.schedule)}</div>}
      </td>
      <td className="hide-sm muted small">{scheduleText(j.schedule)}</td>
      <td className="small nowrap">
        {lastEnd ? <span title={fmtDateTime(lastEnd / 1000)}>{ago(lastEnd / 1000)}</span> : <span className="faint">never</span>}
        {dur !== null && <div className="faint xsmall">took {durLabel}</div>}
      </td>
      <td className="hide-md small job-out-cell">
        {out ? <span className={`job-out ${out.ok ? '' : 'bad'}`} title={`${fmtDateTime(out.end / 1000)} — ${out.summary}`}>{out.summary || (out.ok ? '' : `failed: ${out.result}`)}</span> : <span className="faint">—</span>}
        {out && <div className="faint xsmall">{ago(out.end / 1000)}</div>}
      </td>
      <td className="hide-sm small nowrap">{j.status === 'paused' ? <span className="faint">paused</span> : until(j.next)}</td>
      <td className="right hide-md tnum small nowrap">{j.week.runs.toLocaleString()}{j.week.failed > 0 && <span className="job-fail-n"> ({j.week.failed})</span>}</td>
      <td className="right nowrap" onClick={e => e.stopPropagation()}>
        {owner && j.control && j.status !== 'running' && (
          <button type="button" className="btn ghost xs icon-only" title="Run now" aria-label={`Run ${j.name} now`} onClick={() => onAct('run')}><Icon name="play" size={13} /></button>
        )}
        <MenuButton label={`Options for ${j.name}`} align="right" className="btn ghost xs icon-only" items={() => jobMenu(j, owner, onOpen, onAct, onMute)} width={230}><Icon name="more" size={14} /></MenuButton>
      </td>
    </tr>
  )
}

// ---------- activity: runs that did something, across jobs ----------

function Activity({ server, jobs, onOpen }: { server: Server; jobs: Job[]; onOpen: (unit: string) => void }) {
  const { version } = useApp()
  const [failedOnly, setFailedOnly] = useState(false)
  const runs = useAsync(() => api.get<JobRun[]>(`${V1}/servers/${server.id}/jobs/runs${qs({ busy: 1, failed: failedOnly ? 1 : undefined, limit: 300 })}`), [server.id, version, failedOnly])
  const names = useMemo(() => new Map(jobs.map(j => [j.unit, j.name])), [jobs])
  const byDay = useMemo(() => {
    const m = new Map<string, JobRun[]>()
    for (const r of runs.data || []) {
      const d = new Date(r.end).toLocaleDateString('en-GB', { weekday: 'long', day: 'numeric', month: 'long' })
      m.set(d, [...(m.get(d) || []), r])
    }
    return [...m.entries()]
  }, [runs.data])
  return (
    <Panel title="What the jobs did" icon="activity" flush actions={<label className="logs-check"><Switch size="sm" checked={failedOnly} onChange={setFailedOnly} label="Only failures" />Only failures</label>}>
      {runs.loading && !runs.data ? <Loading /> : !byDay.length ? (
        <Empty title={failedOnly ? 'No failures' : 'Nothing yet'} icon="check-circle">{failedOnly ? 'No job has failed in the kept history.' : 'Runs that write something or fail show up here. Quiet runs only count.'}</Empty>
      ) : (
        <div className="job-feed">
          {byDay.map(([day, list]) => (
            <div key={day}>
              <div className="job-feed-day">{day}</div>
              {list.map(r => (
                <button type="button" key={r.invocation} className={`job-feed-row ${r.ok ? '' : 'bad'}`} onClick={() => onOpen(r.unit)}>
                  <span className="job-feed-time tnum">{new Date(r.end).toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' })}</span>
                  <span className={`job-dot ${r.ok ? (r.warnings ? 'warn' : 'ok') : 'crit'}`} />
                  <span className="job-feed-name ellipsis">{names.get(r.unit) || r.unit.replace(/\.timer$/, '')}</span>
                  <span className="job-feed-msg ellipsis">{r.summary || (r.ok ? '' : `failed (${r.result}${r.exit_status ? `, exit ${r.exit_status}` : ''})`)}</span>
                </button>
              ))}
            </div>
          ))}
        </div>
      )}
    </Panel>
  )
}

// ---------- one job ----------

type DetailTab = 'runs' | 'log' | 'settings'

function JobDetail({ server, job, owner, groups, onClose, onAct, onSave }: { server: Server; job: Job; owner: boolean; groups: string[]; onClose: () => void; onAct: (a: 'run' | 'pause' | 'resume') => void; onSave: (p: Partial<Pick<Job, 'label' | 'group' | 'notes' | 'muted'>>) => Promise<boolean> }) {
  const [tab, setTab] = useState<DetailTab>('runs')
  const tabs: { id: DetailTab; label: string; icon: string }[] = [{ id: 'runs', label: 'Runs', icon: 'list' }]
  if (owner && !job.app) tabs.push({ id: 'log', label: 'Log', icon: 'terminal' })
  if (owner) tabs.push({ id: 'settings', label: 'Settings', icon: 'settings' })
  return (
    <Modal size="lg" onClose={onClose} title={<span className="row">{job.name}<StatusBadge s={job.status} /></span>}
      footer={owner && job.app ? <>
        {job.url && <a className="btn" href={job.url} target="_blank" rel="noopener"><Icon name="external" size={14} />Settings in {job.app_name}</a>}
        <span className="spacer" />
        {job.control && <button className="btn primary" disabled={job.status === 'running'} onClick={() => onAct('run')}><Icon name="play" size={14} />Run now</button>}
      </> : owner && job.control ? <>
        {job.status === 'paused'
          ? <button className="btn" onClick={() => onAct('resume')}><Icon name="repeat" size={14} />Resume schedule</button>
          : <button className="btn" onClick={() => onAct('pause')}><Icon name="pause" size={14} />Pause schedule</button>}
        <span className="spacer" />
        <button className="btn primary" disabled={job.status === 'running'} onClick={() => onAct('run')}><Icon name="play" size={14} />Run now</button>
      </> : undefined}>
      <div className="stack">
        {job.problem && <div className="callout danger"><Icon name="alert" size={15} /><span>{job.problem}</span></div>}
        <dl className="job-facts">
          <div><dt>What</dt><dd>{job.description || '—'}</dd></div>
          <div><dt>Schedule</dt><dd>{scheduleText(job.schedule)}<div className="faint xsmall mono">{(job.schedule || []).join(' · ')}</div></dd></div>
          <div><dt>Next run</dt><dd>{job.status === 'paused' ? 'paused' : job.next ? `${until(job.next)} · ${fmtDateTime(job.next / 1000)}` : '—'}</dd></div>
          <div><dt>Last 7 days</dt><dd>{plural(job.week.runs, 'run')}, {job.week.failed} failed{job.week.avg_ms ? ` · usually ${durText(job.week.avg_ms)}, longest ${durText(job.week.max_ms)}` : ''}</dd></div>
          {job.app
            ? <div><dt>Runs in</dt><dd>{job.app_name} <span className="faint mono small">({job.unit})</span>{job.status === 'paused' && <div className="faint xsmall">Switched off in {job.app_name}'s settings.</div>}</dd></div>
            : <div><dt>Unit</dt><dd className="mono small">{job.unit} → {job.service} <span className="faint">({job.scope})</span></dd></div>}
          {job.notes && <div><dt>Notes</dt><dd className="pre-wrap">{job.notes}</dd></div>}
        </dl>
        {tabs.length > 1 && <Tabs tabs={tabs} value={tab} onChange={setTab} />}
        {tab === 'runs' && <Runs server={server} job={job} owner={owner} />}
        {tab === 'log' && <JobLog server={server} job={job} />}
        {tab === 'settings' && <JobSettings job={job} groups={groups} onSave={onSave} />}
      </div>
    </Modal>
  )
}

function Runs({ server, job, owner }: { server: Server; job: Job; owner: boolean }) {
  const { version } = useApp()
  const [busy, setBusy] = useState(true)
  const runs = useAsync(() => api.get<JobRun[]>(`${V1}/servers/${server.id}/jobs/runs${qs({ unit: job.unit, busy: busy ? 1 : undefined, limit: 100 })}`), [server.id, job.unit, busy, version])
  const [open, setOpen] = useState<string | null>(null)
  return (
    <div className="stack tight">
      <label className="logs-check"><Switch size="sm" checked={busy} onChange={setBusy} label="Only runs that did something" />Only runs that wrote something or failed <span className="faint">(quiet runs are kept a day)</span></label>
      {runs.loading && !runs.data ? <Loading /> : !runs.data?.length ? <Empty title="No runs kept" icon="clock">{busy ? 'Every recent run finished quietly.' : 'Nothing recorded yet.'}</Empty> : (
        <ul className="job-runs">
          {runs.data.map(r => (
            <li key={r.invocation}>
              <button type="button" className={`job-run ${r.ok ? '' : 'bad'}`} onClick={() => owner && !job.app && setOpen(open === r.invocation ? null : r.invocation)} disabled={!owner || !!job.app} title={owner && !job.app ? 'Show this run’s log' : undefined}>
                <span className={`job-dot ${r.ok ? (r.warnings ? 'warn' : 'ok') : 'crit'}`} />
                <span className="tnum small nowrap">{fmtDateTime(r.end / 1000)}</span>
                <span className="faint xsmall nowrap">{r.start ? durText(r.end - r.start) : ''}</span>
                <span className="small ellipsis grow">{r.summary || (r.ok ? <span className="faint">no output</span> : `failed (${r.result}${r.exit_status ? `, exit ${r.exit_status}` : ''})`)}</span>
                {r.lines > 1 && <span className="faint xsmall nowrap">{r.lines} lines</span>}
              </button>
              {open === r.invocation && <JobLog server={server} job={job} invocation={r.invocation} compact />}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function JobLog({ server, job, invocation, compact }: { server: Server; job: Job; invocation?: string; compact?: boolean }) {
  const [tail, setTail] = useState(300)
  const [system, setSystem] = useState(!!invocation)
  const log = useAsync(() => api.get<{ lines: JobLogLine[] }>(`${V1}/servers/${server.id}/jobs/log${qs({ unit: job.unit, invocation, tail })}`), [server.id, job.unit, invocation, tail])
  const lines = (log.data?.lines || []).filter(l => system || !l.sys)
  let prev = ''
  return (
    <div className={`job-log ${compact ? 'compact' : ''}`}>
      {!compact && (
        <div className="logs-tools">
          <label className="logs-check"><Switch size="sm" checked={system} onChange={setSystem} label="Show systemd’s lines" />systemd’s lines</label>
          <span className="spacer" />
          <Segmented size="sm" label="Lines" value={tail} onChange={setTail} options={[100, 300, 1000, 5000].map(n => ({ value: n, label: n >= 1000 ? `${n / 1000}k` : String(n) }))} />
          <button type="button" className="btn ghost sm icon-only" aria-label="Reload" title="Reload" onClick={() => log.reload()}><Icon name="refresh" size={14} /></button>
        </div>
      )}
      {log.loading && !log.data ? <Loading /> : log.error ? <div className="callout danger"><Icon name="alert" size={15} /><span>{log.error}</span></div> : !lines.length ? <div className="logs-note">Nothing logged.</div> : (
        <div className="logs-body wrap" role="log">
          {lines.map((l, i) => {
            const sep = !invocation && l.i !== prev && i > 0
            prev = l.i
            return (
              <div key={i} className={`log-line ${l.p <= 3 ? 'error' : l.p === 4 ? 'warn' : ''} ${l.sys ? 'sys' : ''} ${sep ? 'sep' : ''}`}>
                <span className="log-ts">{new Date(l.t).toLocaleString('en-GB', { day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit', second: '2-digit' })}</span>
                <span className="log-msg">{l.m}</span>
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}

function JobSettings({ job, groups, onSave }: { job: Job; groups: string[]; onSave: (p: Partial<Pick<Job, 'label' | 'group' | 'notes' | 'muted'>>) => Promise<boolean> }) {
  const { toast } = useToast()
  const [f, setF] = useState({ label: job.label, group: job.group, notes: job.notes, muted: job.muted })
  const [saving, setSaving] = useState(false)
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    if (await onSave(f)) toast('Saved')
    setSaving(false)
  }
  return (
    <form className="stack" onSubmit={submit}>
      <Field label="Name" help={`Shown instead of ${job.unit.replace(/\.timer$/, '')}`}><input value={f.label} onChange={e => setF({ ...f, label: e.target.value })} placeholder={job.unit.replace(/\.timer$/, '')} maxLength={80} /></Field>
      <Field label="Group"><input value={f.group} onChange={e => setF({ ...f, group: e.target.value })} list="job-groups" maxLength={60} /></Field>
      <datalist id="job-groups">{groups.map(g => <option key={g} value={g} />)}</datalist>
      <Field label="Notes" help="What it’s for, what to do when it fails."><textarea rows={3} value={f.notes} onChange={e => setF({ ...f, notes: e.target.value })} maxLength={2000} /></Field>
      <label className="logs-check"><Switch checked={!f.muted} onChange={v => setF({ ...f, muted: !v })} label="Alert when it fails" />Alert when it fails, hangs or stops being scheduled</label>
      <div className="row"><span className="spacer" /><button className="btn primary" disabled={saving}>Save</button></div>
    </form>
  )
}
