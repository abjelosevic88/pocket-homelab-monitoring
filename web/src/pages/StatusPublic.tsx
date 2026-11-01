// The public status page: no sign-in, only the checks marked public, read
// from the same Uptime Kuma–shaped JSON other apps use.
import { useEffect, useState } from 'react'
import { useParams } from 'react-router-dom'
import { BeatBar, fmtUptime } from './server/Uptime'

interface PageJSON { config: { title: string; description: string }; publicGroupList: { id: number; name: string; monitorList: { id: number; name: string }[] }[] }
interface BeatsJSON { heartbeatList: Record<string, { status: number; time: string; ping: number | null }[]>; uptimeList: Record<string, number> }

const parse = (s: string) => Math.floor(Date.parse(s.replace(' ', 'T') + 'Z') / 1000)

export default function StatusPublic() {
  const { slug } = useParams()
  const [page, setPage] = useState<PageJSON | null>(null)
  const [beats, setBeats] = useState<BeatsJSON | null>(null)
  const [err, setErr] = useState('')
  const [at, setAt] = useState(new Date())
  useEffect(() => {
    let stop = false
    const load = async () => {
      try {
        const [p, b] = await Promise.all([fetch(`/api/status-page/${slug}`), fetch(`/api/status-page/heartbeat/${slug}`)])
        if (!p.ok) throw new Error(p.status === 404 ? 'There is no status page here.' : `HTTP ${p.status}`)
        const pj = await p.json(), bj = await b.json()
        if (!stop) { setPage(pj); setBeats(bj); setErr(''); setAt(new Date()) }
      } catch (e) { if (!stop) setErr((e as Error).message) }
    }
    load()
    const t = setInterval(() => { if (!document.hidden) load() }, 60_000)
    return () => { stop = true; clearInterval(t) }
  }, [slug])
  useEffect(() => { if (page) document.title = page.config.title }, [page])

  if (err && !page) return <div className="status-pub"><p className="muted">{err}</p></div>
  if (!page || !beats) return <div className="status-pub"><p className="muted">Loading…</p></div>
  const last = (id: number) => { const l = beats.heartbeatList[String(id)] || []; return l.length ? l[l.length - 1].status : -1 }
  const all = page.publicGroupList.flatMap(g => g.monitorList)
  const down = all.filter(m => last(m.id) === 0)
  return (
    <div className="status-pub">
      <header>
        <img src="/favicon.svg" alt="" width={36} height={36} />
        <div><h1>{page.config.title}</h1>{page.config.description && <p className="muted">{page.config.description}</p>}</div>
      </header>
      <div className={`status-banner ${down.length ? 'bad' : 'ok'}`}>
        {down.length ? `${down.map(m => m.name).join(', ')} ${down.length === 1 ? 'is' : 'are'} down` : 'All systems operational'}
      </div>
      {page.publicGroupList.map(g => (
        <section key={g.id} className="status-group">
          <h2>{g.name}</h2>
          <ul>
            {g.monitorList.map(m => {
              const st = last(m.id)
              return (
                <li key={m.id}>
                  <span className={`up-dot ${['down', 'up', 'pending', 'maint'][st] || 'waiting'}`} />
                  <span className="grow strong ellipsis">{m.name}</span>
                  <BeatBar beats={(beats.heartbeatList[String(m.id)] || []).map(b => ({ s: b.status, t: parse(b.time), ms: b.ping }))} n={40} />
                  <span className="tnum up-num">{fmtUptime(beats.uptimeList[`${m.id}_24`])}</span>
                </li>
              )
            })}
          </ul>
        </section>
      ))}
      <footer className="muted xsmall">Updated {at.toLocaleTimeString()} · uptime over the last 24 hours · Pocket Monitoring</footer>
    </div>
  )
}
