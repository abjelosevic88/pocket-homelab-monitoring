// Every server I can see, one card each.
import { Link } from 'react-router-dom'
import { useApp } from '../lib/app-context'
import { ago, fmtDuration, fmtPct, fmtRate, fmtTemp, level, tempLevel } from '../lib/format'
import type { Server } from '../lib/types'
import { Meter, Sparkline } from '../components/Chart'
import { Icon } from '../components/icons'
import { openAddServer, useServerActions } from '../components/ServerDialogs'
import { serverSubtitle, ServerState } from '../components/Status'
import { Shell } from '../components/Shell'
import { Empty, Loading, MenuButton } from '../components/ui'

export default function Overview() {
  const { servers, serversLoaded, alerts } = useApp()
  const actions = useServerActions()
  const up = servers.filter(s => s.status === 'up').length
  const down = servers.filter(s => s.status === 'down').length
  return (
    <Shell title="Overview" sub={servers.length ? `${up} online${down ? ` · ${down} offline` : ''}${alerts.length ? ` · ${alerts.length} alert${alerts.length === 1 ? '' : 's'}` : ''}` : undefined}>
      <div className="page-inner">
        {!serversLoaded ? <Loading /> : !servers.length ? (
          <Empty title="No servers yet" icon="server">
            <p>Add this machine’s neighbours: run the Pocket agent on them, or point at a Glances server.</p>
            <button className="btn primary mt-s" onClick={openAddServer}><Icon name="plus" size={15} />Add server</button>
          </Empty>
        ) : (
          <div className="ov-grid">
            {servers.map(s => <ServerCard key={s.id} s={s} menu={actions.items(s)} />)}
            <button type="button" className="ov-add" onClick={openAddServer}><Icon name="plus" size={20} /><span>Add server</span></button>
          </div>
        )}
      </div>
      {actions.dialogs}
    </Shell>
  )
}

function ServerCard({ s, menu }: { s: Server; menu: ReturnType<ReturnType<typeof useServerActions>['items']> }) {
  const { settings } = useApp()
  const unit = settings?.temp_unit || 'C'
  const h = s.summary
  const live = s.status === 'up' && !!h
  return (
    <article className={`ov-card ${s.status}`} style={{ ['--c' as string]: s.color }}>
      <header className="ov-head">
        <span className="srv-swatch lg" />
        <Link to={`/server/${s.id}`} className="ov-title">
          <h2 className="ellipsis">{s.name}</h2>
          <span className="muted small ellipsis">{serverSubtitle(s)}</span>
        </Link>
        <ServerState status={s.status} label size="md" />
        <MenuButton label={`Options for ${s.name}`} align="right" items={menu} width={210}><Icon name="more" size={16} /></MenuButton>
      </header>
      {!h ? (
        <Link to={`/server/${s.id}`} className="ov-wait">
          {s.status === 'paused' ? 'Paused — not being read.' : s.last_error ? <span className="danger-text">{s.last_error}</span> : 'Waiting for the first reading…'}
        </Link>
      ) : (
        <Link to={`/server/${s.id}`} className={`ov-body ${live ? '' : 'stale'}`}>
          {!live && <div className="ov-stale"><Icon name="alert" size={13} />Last reading {ago(s.last_seen || h.time)}{s.last_error ? ` — ${s.last_error}` : ''}</div>}
          <div className="ov-meters">
            <MeterRow label="CPU" value={fmtPct(h.cpu)} pct={h.cpu} extra={h.threads ? `${h.threads} threads` : undefined} />
            <MeterRow label="Memory" value={fmtPct(h.mem_pct)} pct={h.mem_pct} />
            <MeterRow label="Disk" value={fmtPct(h.fs_pct)} pct={h.fs_pct} extra={h.fs_mount} warn={80} />
            {h.temp > 0 && <MeterRow label="Temp" value={fmtTemp(h.temp, unit)} pct={Math.min(100, h.temp)} extra={h.temp_label} lvl={tempLevel(h.temp)} />}
          </div>
          <div className="ov-spark">
            <span className="ov-spark-label muted xsmall">CPU, last hour</span>
            <Sparkline values={s.spark || []} width={260} height={38} label={`CPU over the last hour, now ${fmtPct(h.cpu)}`} />
          </div>
          <dl className="ov-facts">
            <div><dt><Icon name="network" size={13} />Network</dt><dd className="tnum">↓ {fmtRate(h.rx_bps)} · ↑ {fmtRate(h.tx_bps)}</dd></div>
            <div><dt><Icon name="disk" size={13} />Disk I/O</dt><dd className="tnum">R {fmtRate(h.read_bps)} · W {fmtRate(h.write_bps)}</dd></div>
            <div><dt><Icon name="clock" size={13} />Uptime</dt><dd className="tnum">{fmtDuration(h.uptime_s)}{h.load1 ? <span className="muted"> · load {h.load1.toFixed(2)}</span> : null}</dd></div>
            {h.containers_total > 0 && <div><dt><Icon name="box" size={13} />Containers</dt><dd className="tnum">{h.containers_running}/{h.containers_total} running
              {h.containers_unhealthy > 0 && <span className="lvl-badge crit sm"><Icon name="alert" size={11} />{h.containers_unhealthy} unhealthy</span>}</dd></div>}
          </dl>
          {s.alerts_active > 0 && <div className="ov-alerts"><Icon name="bell" size={13} />{s.alerts_active} active alert{s.alerts_active === 1 ? '' : 's'}</div>}
        </Link>
      )}
    </article>
  )
}

function MeterRow({ label, value, pct, extra, warn = 70, lvl }: { label: string; value: string; pct: number; extra?: string; warn?: number; lvl?: ReturnType<typeof level> }) {
  const l = lvl || level(pct, warn)
  return (
    <div className="ov-meter">
      <span className="ov-meter-label">{label}</span>
      <Meter value={pct} lvl={l} label={`${label} ${value}`} thin showLevel={false} />
      <span className="ov-meter-val tnum">{value}{l !== 'ok' && <Icon name="alert" size={12} className={`lvl-ico ${l}`} title={l === 'crit' ? 'Critical' : 'High'} />}</span>
      {extra && <span className="ov-meter-extra muted xsmall ellipsis">{extra}</span>}
    </div>
  )
}
