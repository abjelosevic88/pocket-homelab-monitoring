// Active alerts and the history of every alert on servers I can see.
import { Link } from 'react-router-dom'
import { api, V1 } from '../lib/api'
import { useApp } from '../lib/app-context'
import { ago, fmtDateTime, fmtDuration, secondsBetween } from '../lib/format'
import type { Alert } from '../lib/types'
import { Icon } from '../components/icons'
import { Shell } from '../components/Shell'
import { Empty, Loading, Segmented, useAsync } from '../components/ui'
import { useState } from 'react'

export const METRIC_ICON: Record<string, string> = { cpu: 'cpu', mem: 'memory', swap: 'memory', load: 'gauge', disk: 'disk', temp: 'thermometer', offline: 'power', container: 'box', raid: 'shield' }

export default function AlertsPage() {
  const { alerts, version } = useApp()
  const [tab, setTab] = useState<'active' | 'all'>('active')
  const history = useAsync(() => api.get<Alert[]>(`${V1}/alerts?state=all&limit=300`), [version, tab])
  const list = tab === 'active' ? alerts : (history.data || [])
  return (
    <Shell title="Alerts" sub={alerts.length ? `${alerts.length} active` : 'All quiet'}
      controls={<Segmented size="sm" label="Show" value={tab} onChange={setTab} options={[{ value: 'active', label: `Active${alerts.length ? ` (${alerts.length})` : ''}` }, { value: 'all', label: 'History' }]} />}>
      <div className="page-inner narrow">
        {tab === 'all' && !history.data ? <Loading /> : !list.length ? (
          <Empty title={tab === 'active' ? 'Nothing is wrong right now' : 'No alerts yet'} icon="check-circle">
            {tab === 'active' ? 'Alerts appear here when a rule fires. ' : ''}<Link to="/settings/rules">Alert rules</Link>
          </Empty>
        ) : <AlertList alerts={list} showServer />}
      </div>
    </Shell>
  )
}

export function AlertList({ alerts, showServer }: { alerts: Alert[]; showServer?: boolean }) {
  const { h12 } = useApp()
  return (
    <ul className="alert-list">
      {alerts.map(a => {
        const open = !a.resolved_at
        return (
          <li key={a.id} className={`alert-row ${a.severity} ${open ? 'open' : 'resolved'}`}>
            <span className={`alert-ico ${open ? a.severity : 'ok'}`}><Icon name={open ? (a.severity === 'critical' ? 'alert' : METRIC_ICON[a.metric] || 'alert') : 'check-circle'} size={16} /></span>
            <div className="grow">
              <div className="alert-msg">
                <span className={`lvl-badge ${open ? (a.severity === 'critical' ? 'crit' : 'warn') : 'ok'} sm`}>{open ? (a.severity === 'critical' ? 'Critical' : 'Warning') : 'Resolved'}</span>
                <span>{a.message}</span>
              </div>
              <div className="muted small">
                {showServer && <><Link to={`/server/${a.server_id}`}>{a.server_name}</Link> · </>}
                <span title={fmtDateTime(a.started_at, h12)}>started {ago(a.started_at)}</span>
                {a.resolved_at ? <> · <span title={fmtDateTime(a.resolved_at, h12)}>resolved {ago(a.resolved_at)}</span> · lasted {fmtDuration(secondsBetween(a.started_at, a.resolved_at))}</> : <> · going on for {fmtDuration(secondsBetween(a.started_at))}</>}
              </div>
            </div>
          </li>
        )
      })}
    </ul>
  )
}
