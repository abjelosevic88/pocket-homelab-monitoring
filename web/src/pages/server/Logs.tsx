// Container logs: pick a container (or all of them, merged by time), read the
// tail, follow new lines live, filter, download.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api, ApiError, qs, V1 } from '../../lib/api'
import { useApp } from '../../lib/app-context'
import type { Container, LogLine, Logs, Server, Snapshot } from '../../lib/types'
import { Icon } from '../../components/icons'
import { Select } from '../../components/Select'
import { Empty, Loading, Segmented, Switch, errMsg, useIsMobile } from '../../components/ui'
import { ContainerState, Search } from './Tables'

const ALL = ''
const MAX_LINES = 5000
const TAILS = [200, 500, 1000, 5000]
const FOLLOW_MS = 2000

// ANSI colour codes and other escape sequences: dropped, not rendered.
// eslint-disable-next-line no-control-regex
const ANSI = /\u001b\[[0-9;?]*[ -/]*[@-~]|\u001b\][^\u0007]*\u0007/g

type Level = 'error' | 'warn' | ''
const ERR = /\b(error|err|fatal|panic|exception|traceback|critical|crit|failed|failure)\b/i
const WARN = /\b(warn|warning)\b/i

function levelOf(l: LogLine): Level {
  if (ERR.test(l.m)) return 'error'
  if (WARN.test(l.m)) return 'warn'
  return ''
}

interface Row extends LogLine { key: number; text: string; lvl: Level }

let seq = 0
function toRows(lines: LogLine[]): Row[] {
  return lines.map(l => { const text = l.m.replace(ANSI, ''); return { ...l, m: text, text, key: ++seq, lvl: levelOf({ ...l, m: text }) } })
}

function fmtTs(t: string, h12: boolean, withDate: boolean) {
  const d = new Date(t)
  if (isNaN(d.getTime())) return ''
  const time = d.toLocaleTimeString(h12 ? 'en-US' : 'en-GB', { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: h12 })
  const ms = String(d.getMilliseconds()).padStart(3, '0')
  return (withDate ? d.toLocaleDateString('en-GB', { day: '2-digit', month: 'short' }) + ' ' : '') + time + '.' + ms
}

/** Stable per-name hue index for the merged view (identity only, the name is always printed). */
function hueOf(name: string) {
  let h = 0
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) >>> 0
  return h % 4
}

export function LogsView({ server, snap, initial, onPick }: { server: Server; snap: Snapshot; initial: string; onPick: (name: string) => void }) {
  const { settings } = useApp()
  const mobile = useIsMobile()
  const h12 = settings?.time_format === '12h'
  const [container, setContainer] = useState(initial)
  const [tail, setTail] = useState(500)
  const [follow, setFollow] = useState(true)
  const [rows, setRows] = useState<Row[]>([])
  const [truncated, setTruncated] = useState(false)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<{ status: number; msg: string } | null>(null)
  const [q, setQ] = useState('')
  const [onlyMatches, setOnlyMatches] = useState(true)
  const [errorsOnly, setErrorsOnly] = useState(false)
  const [showTs, setShowTs] = useState(true)
  const [wrap, setWrap] = useState(true)
  const [pinned, setPinned] = useState(true) // scrolled to the bottom: keep following
  const [list, setList] = useState('')
  const body = useRef<HTMLDivElement>(null)
  const last = useRef('')
  const gen = useRef(0)

  useEffect(() => { setContainer(initial) }, [initial])
  const pick = (name: string) => { setContainer(name); onPick(name) }

  const url = useCallback((since: string) => `${V1}/servers/${server.id}/logs${qs({ container: container || undefined, tail, since: since || undefined })}`, [server.id, container, tail])

  // First load (and on every change of container or tail).
  useEffect(() => {
    const my = ++gen.current
    setLoading(true); setError(null); setRows([]); last.current = ''; setPinned(true)
    api.get<Logs>(url('')).then(r => {
      if (gen.current !== my) return
      setRows(toRows(r.lines)); setTruncated(r.truncated)
      last.current = r.lines.length ? r.lines[r.lines.length - 1].t : r.now
    }).catch(e => { if (gen.current === my) setError({ status: e instanceof ApiError ? e.status : 0, msg: errMsg(e) }) })
      .finally(() => { if (gen.current === my) setLoading(false) })
  }, [url])

  // Follow: ask for what came after the last line.
  useEffect(() => {
    if (!follow || loading || error) return
    const my = gen.current
    let stop = false
    let timer: ReturnType<typeof setTimeout>
    const tick = async () => {
      if (!document.hidden && last.current) {
        try {
          const r = await api.get<Logs>(url(last.current))
          if (stop || gen.current !== my) return
          if (r.lines.length) {
            last.current = r.lines[r.lines.length - 1].t || last.current
            setRows(prev => { const next = prev.concat(toRows(r.lines)); return next.length > MAX_LINES ? next.slice(next.length - MAX_LINES) : next })
          }
        } catch { /* a hiccup: try again next tick */ }
      }
      if (!stop) timer = setTimeout(tick, FOLLOW_MS)
    }
    timer = setTimeout(tick, FOLLOW_MS)
    return () => { stop = true; clearTimeout(timer) }
  }, [follow, loading, error, url])

  const needle = q.trim().toLowerCase()
  const shown = useMemo(() => rows.filter(r => (!errorsOnly || r.lvl === 'error' || r.s === 'err') && (!needle || !onlyMatches || r.text.toLowerCase().includes(needle) || r.c.toLowerCase().includes(needle))), [rows, errorsOnly, needle, onlyMatches])
  const matches = useMemo(() => needle ? rows.filter(r => r.text.toLowerCase().includes(needle)).length : 0, [rows, needle])

  // Stay at the bottom while pinned.
  useEffect(() => { if (pinned && body.current) body.current.scrollTop = body.current.scrollHeight }, [shown, pinned])
  const onScroll = () => {
    const el = body.current
    if (!el) return
    setPinned(el.scrollHeight - el.scrollTop - el.clientHeight < 40)
  }
  const toBottom = () => { setPinned(true); if (body.current) body.current.scrollTop = body.current.scrollHeight }

  const save = () => {
    const text = shown.map(r => `${r.t}${container ? '' : ` [${r.c}]`}${r.s === 'err' ? ' [stderr]' : ''} ${r.text}`).join('\n') + '\n'
    const a = document.createElement('a')
    a.href = URL.createObjectURL(new Blob([text], { type: 'text/plain' }))
    a.download = `${server.name}-${container || 'all-containers'}-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-')}.log`
    a.click()
    setTimeout(() => URL.revokeObjectURL(a.href), 5000)
  }

  const containers = useMemo(() => [...(snap.containers || [])].sort((a, b) => (a.state === 'running' ? 0 : 1) - (b.state === 'running' ? 0 : 1) || a.name.localeCompare(b.name)), [snap.containers])
  const current = containers.find(c => c.name === container || c.id === container)
  const ln = list.trim().toLowerCase()
  const listed = containers.filter(c => !ln || `${c.name} ${c.project || ''} ${c.image}`.toLowerCase().includes(ln))
  const multi = !container

  if (server.kind === 'glances') {
    return <Empty title="Logs come through the agent" icon="terminal">Glances doesn’t serve container logs. Run the Pocket Monitoring agent on this machine (Settings → Servers) and add it as an agent to read them here.</Empty>
  }
  if (server.role !== 'owner') {
    return <Empty title="Only the owner reads logs" icon="lock">Container logs often hold tokens and passwords, so they stay with the person who added this server.</Empty>
  }
  if (!containers.length) {
    return <Empty title="No containers here" icon="box">There’s no container engine on this machine, or it can’t read the Docker socket.</Empty>
  }

  const picker = mobile ? (
    <Select searchable label="Container" value={container} onChange={pick} width={300}
      options={[{ value: ALL, label: 'All running containers' }, ...containers.map(c => ({ value: c.name, label: c.name, hint: c.state === 'running' ? '' : c.state }))]} />
  ) : null

  return (
    <div className={`logs ${mobile ? 'mobile' : ''}`}>
      {!mobile && (
        <aside className="logs-list" aria-label="Containers">
          <div className="logs-list-search"><Search value={list} onChange={setList} placeholder="Find a container" /></div>
          <ul>
            <li><button type="button" className={`logs-item ${multi ? 'active' : ''}`} onClick={() => pick(ALL)}>
              <Icon name="layers" size={14} /><span className="ellipsis grow">All running containers</span>
              <span className="faint small tnum">{containers.filter(c => c.state === 'running').length}</span>
            </button></li>
            {listed.map(c => <ContainerItem key={c.id} c={c} active={!multi && (c.name === container || c.id === container)} onClick={() => pick(c.name)} />)}
          </ul>
        </aside>
      )}
      <section className="logs-main">
        <div className="logs-head">
          {picker || (
            <div className="logs-title">
              <Icon name={multi ? 'layers' : 'terminal'} size={15} className="muted" />
              <h3 className="ellipsis">{multi ? 'All running containers' : current?.name || container}</h3>
              {current && <ContainerState c={current} />}
            </div>
          )}
          <span className="spacer" />
          <span className={`logs-live ${follow ? 'on' : ''}`} aria-live="polite">
            <span className="dot" />{follow ? 'Following' : 'Paused'}
          </span>
          <button type="button" className="btn sm" onClick={() => setFollow(f => !f)} title={follow ? 'Stop following' : 'Follow new lines'}>
            <Icon name={follow ? 'pause' : 'play'} size={13} />{follow ? 'Pause' : 'Follow'}
          </button>
          <button type="button" className="btn ghost sm icon-only" onClick={save} disabled={!shown.length} aria-label="Download what’s shown" title="Download what’s shown"><Icon name="download" size={15} /></button>
        </div>
        <div className="logs-tools">
          <Search value={q} onChange={setQ} placeholder={multi ? 'Filter lines or container names' : 'Filter lines'} />
          {needle && (
            <label className="logs-check"><Switch size="sm" checked={onlyMatches} onChange={setOnlyMatches} label="Only matching lines" />Only matches <span className="faint tnum">{matches}</span></label>
          )}
          <label className="logs-check"><Switch size="sm" checked={errorsOnly} onChange={setErrorsOnly} label="Errors only" />Errors</label>
          <label className="logs-check"><Switch size="sm" checked={showTs} onChange={setShowTs} label="Show times" />Times</label>
          <label className="logs-check"><Switch size="sm" checked={wrap} onChange={setWrap} label="Wrap long lines" />Wrap</label>
          <span className="spacer" />
          <Segmented size="sm" label="Lines" value={tail} onChange={setTail} options={TAILS.map(n => ({ value: n, label: n >= 1000 ? `${n / 1000}k` : String(n), title: `Last ${n} lines` }))} />
        </div>
        <div className="logs-body-wrap">
          {loading ? <Loading /> : error ? (
            <Empty title={error.status === 403 ? 'Not yours to read' : 'Couldn’t read the logs'} icon="alert">{error.msg}</Empty>
          ) : !rows.length ? (
            <Empty title="Nothing logged yet" icon="terminal">{follow ? 'New lines show up here as they’re written.' : 'This container hasn’t written anything.'}</Empty>
          ) : (
            <div ref={body} className={`logs-body ${wrap ? 'wrap' : ''}`} onScroll={onScroll} role="log" aria-label="Log lines" tabIndex={0}>
              {truncated && <div className="logs-note">Showing the last {tail.toLocaleString()} lines{multi ? ' across all containers' : ''}. Pick more lines above to go further back.</div>}
              {shown.length === 0 && <div className="logs-note">No line matches.</div>}
              {shown.map(r => (
                <div key={r.key} className={`log-line ${r.s === 'err' ? 'stderr' : ''} ${r.lvl}`}>
                  {showTs && <span className="log-ts" title={r.t}>{fmtTs(r.t, h12, multi && tail > 1000)}</span>}
                  {multi && <button type="button" className={`log-ctr h${hueOf(r.c)}`} title={`Only ${r.c}`} onClick={() => pick(r.c)}>{r.c}</button>}
                  <span className="log-msg">{needle ? <Mark text={r.text} needle={needle} /> : r.text}</span>
                </div>
              ))}
            </div>
          )}
          {!pinned && !loading && rows.length > 0 && (
            <button type="button" className="btn sm logs-jump" onClick={toBottom}><Icon name="chevron-down" size={14} />Latest</button>
          )}
        </div>
        <div className="logs-foot muted small">
          <span className="tnum">{shown.length.toLocaleString()} line{shown.length === 1 ? '' : 's'}{shown.length !== rows.length ? ` of ${rows.length.toLocaleString()}` : ''}</span>
          <span><span className="stderr-key" /> written to stderr</span>
        </div>
      </section>
    </div>
  )
}

function ContainerItem({ c, active, onClick }: { c: Container; active: boolean; onClick: () => void }) {
  const st = c.health === 'unhealthy' ? 'bad' : c.state === 'running' ? 'ok' : 'off'
  return (
    <li><button type="button" className={`logs-item ${active ? 'active' : ''} ${c.state === 'running' ? '' : 'dim'}`} onClick={onClick} title={`${c.name} · ${c.status}`}>
      <span className={`logs-dot ${st}`} aria-label={c.health || c.state} role="img" />
      <span className="ellipsis grow">{c.name}</span>
      {c.project && c.project !== c.name && <span className="faint xsmall ellipsis logs-proj">{c.project}</span>}
    </button></li>
  )
}

function Mark({ text, needle }: { text: string; needle: string }) {
  const out: React.ReactNode[] = []
  const lower = text.toLowerCase()
  let i = 0
  while (i < text.length) {
    const j = lower.indexOf(needle, i)
    if (j < 0) { out.push(text.slice(i)); break }
    if (j > i) out.push(text.slice(i, j))
    out.push(<mark key={j}>{text.slice(j, j + needle.length)}</mark>)
    i = j + needle.length
  }
  return <>{out}</>
}
