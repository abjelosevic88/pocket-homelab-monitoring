// Every drive on every server I can see: failing and missing ones first,
// grouped by server. A card opens the drive; the bar imports Scrutiny's history.
import { useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { api, V1 } from '../lib/api'
import { useApp } from '../lib/app-context'
import type { DriveView } from '../lib/types'
import { Icon } from '../components/icons'
import { Shell } from '../components/Shell'
import { Empty, errMsg, Loading, MenuButton, Segmented, useAsync } from '../components/ui'
import { DriveDetail, DriveGrid, driveState, ScrutinyImport } from './server/Drives'

export default function DrivesPage() {
  const { servers, version } = useApp()
  const [search, setSearch] = useSearchParams()
  const [importing, setImporting] = useState(false)
  const [group, setGroup] = useState<'server' | 'all'>('server')
  const list = useAsync(() => api.get<DriveView[]>(`${V1}/drives`), [version])
  const open = search.get('drive') || ''
  const openServer = Number(search.get('server')) || 0
  const pick = (v: DriveView | null) => setSearch(v ? { server: String(v.server_id), drive: v.drive.key } : {}, { replace: false })

  const all = list.data || []
  const bad = all.filter(v => driveState(v) === 'failed' || driveState(v) === 'missing').length
  const watch = all.filter(v => driveState(v) === 'warning').length
  const sub = all.length ? `${all.length} drive${all.length === 1 ? '' : 's'}${bad ? ` · ${bad} failing or missing` : ''}${watch ? ` · ${watch} to watch` : ''}${!bad && !watch ? ' · all healthy' : ''}` : undefined
  const owner = servers.some(s => s.role === 'owner')
  const byServer = servers.map(s => ({ s, drives: all.filter(v => v.server_id === s.id) })).filter(x => x.drives.length)

  return (
    <Shell title="Drives" sub={sub}
      controls={!open && <div className="row">
        {all.length > 0 && <Segmented size="sm" label="Group" value={group} onChange={setGroup} options={[{ value: 'server', label: 'By server' }, { value: 'all', label: 'Worst first' }]} />}
        {owner && <MenuButton label="Drive options" align="right" width={240} items={[{ label: 'Import history from Scrutiny…', icon: 'upload', onSelect: () => setImporting(true) }, { label: 'Drive alert rules', icon: 'bell', onSelect: () => { location.href = '/settings/rules' } }]}><Icon name="more" size={16} /></MenuButton>}
      </div>}>
      <div className="page-inner">
        {open && openServer ? <DriveDetail serverId={openServer} driveKey={open} onBack={() => pick(null)} showServer /> :
          !list.data ? (list.error ? <Empty title="Couldn’t load the drives" icon="alert">{errMsg(list.error)}</Empty> : <Loading />) :
          !all.length ? (
            <Empty title="No drive health yet" icon="disk">
              <p>SMART comes from <span className="mono">smartctl</span> in the app and in the agent, which need root and the disks: run their containers privileged (see the README). The first check runs a minute after they start, then every 15 minutes.</p>
            </Empty>
          ) : group === 'all' ? <DriveGrid list={all} onOpen={pick} showServer /> : (
            <div className="stack loose">
              {byServer.map(({ s, drives }) => (
                <section key={s.id} className="drive-sec">
                  <h2><span className="srv-swatch lg" style={{ ['--c' as string]: s.color }} /><Link to={`/server/${s.id}?tab=drives`}>{s.name}</Link><span className="muted small">{drives.length}</span></h2>
                  <DriveGrid list={drives} onOpen={pick} />
                </section>
              ))}
            </div>
          )}
      </div>
      {importing && <ScrutinyImport onClose={() => setImporting(false)} />}
    </Shell>
  )
}
