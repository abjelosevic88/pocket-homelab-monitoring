// Every drive on every server I can see: failing and missing ones first,
// grouped by server, each with its free space on top. A card opens the drive; the bar imports Scrutiny's history.
import { useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { api, V1 } from '../lib/api'
import { useApp } from '../lib/app-context'
import type { DriveView, Snapshot } from '../lib/types'
import { Icon } from '../components/icons'
import { Shell } from '../components/Shell'
import { Empty, errMsg, Loading, MenuButton, Segmented, useAsync } from '../components/ui'
import { DriveDetail, DriveGrid, driveState, ScrutinyImport } from './server/Drives'
import { SpaceGrid, volumes, type Volume } from './server/Space'

export default function DrivesPage() {
  const { servers, version } = useApp()
  const [search, setSearch] = useSearchParams()
  const [importing, setImporting] = useState(false)
  const [group, setGroup] = useState<'server' | 'all'>('server')
  const list = useAsync(() => api.get<DriveView[]>(`${V1}/drives`), [version])
  const on = servers.filter(s => s.enabled).map(s => s.id)
  // free space comes from each server's live snapshot; one that can't answer just shows no space
  const space = useAsync(() => Promise.all(on.map(id => api.get<Snapshot>(`${V1}/servers/${id}/snapshot`).then(sn => [id, volumes(sn.fs || [])] as const).catch(() => [id, [] as Volume[]] as const)))
    .then(rows => new Map<number, Volume[]>(rows)), [version, on.join(',')])
  const open = search.get('drive') || ''
  const openServer = Number(search.get('server')) || 0
  const pick = (v: DriveView | null) => setSearch(v ? { server: String(v.server_id), drive: v.drive.key } : {}, { replace: false })

  const all = list.data || []
  const bad = all.filter(v => driveState(v) === 'failed' || driveState(v) === 'missing').length
  const watch = all.filter(v => driveState(v) === 'warning').length
  const sub = all.length ? `${all.length} drive${all.length === 1 ? '' : 's'}${bad ? ` · ${bad} failing or missing` : ''}${watch ? ` · ${watch} to watch` : ''}${!bad && !watch ? ' · all healthy' : ''}` : undefined
  const owner = servers.some(s => s.role === 'owner')
  const vols = (id: number) => space.data?.get(id) || []
  const byServer = servers.map(s => ({ s, drives: all.filter(v => v.server_id === s.id), vols: vols(s.id) })).filter(x => x.drives.length || x.vols.length)

  return (
    <Shell title="Drives" sub={sub}
      controls={!open && <div className="row">
        {all.length > 0 && <Segmented size="sm" label="Group" value={group} onChange={setGroup} options={[{ value: 'server', label: 'By server' }, { value: 'all', label: 'Worst first' }]} />}
        {owner && <MenuButton label="Drive options" align="right" width={240} items={[{ label: 'Import history from Scrutiny…', icon: 'upload', onSelect: () => setImporting(true) }, { label: 'Drive alert rules', icon: 'bell', onSelect: () => { location.href = '/settings/rules' } }]}><Icon name="more" size={16} /></MenuButton>}
      </div>}>
      <div className="page-inner">
        {open && openServer ? <DriveDetail serverId={openServer} driveKey={open} onBack={() => pick(null)} showServer /> :
          !list.data ? (list.error ? <Empty title="Couldn’t load the drives" icon="alert">{errMsg(list.error)}</Empty> : <Loading />) :
          !all.length && !byServer.length ? (
            <Empty title="No drive health yet" icon="disk">
              <p>SMART comes from <span className="mono">smartctl</span> in the app and in the agent, which need root and the disks: run their containers privileged (see the README). The first check runs a minute after they start, then every 15 minutes.</p>
            </Empty>
          ) : group === 'all' ? (
            <div className="stack loose">
              {byServer.some(x => x.vols.length) && (
                <section className="drive-sec">
                  <h2>Free space</h2>
                  {byServer.filter(x => x.vols.length).map(({ s, vols }) => (
                    <div key={s.id} className="space-row">
                      <div className="muted small row"><span className="srv-swatch sm" style={{ ['--c' as string]: s.color }} />{s.name}</div>
                      <SpaceGrid server={s} list={vols} />
                    </div>
                  ))}
                </section>
              )}
              {all.length > 0 && <section className="drive-sec"><h2>Health</h2><DriveGrid list={all} onOpen={pick} showServer /></section>}
            </div>
          ) : (
            <div className="stack loose">
              {byServer.map(({ s, drives, vols }) => (
                <section key={s.id} className="drive-sec">
                  <h2><span className="srv-swatch lg" style={{ ['--c' as string]: s.color }} /><Link to={`/server/${s.id}?tab=drives`}>{s.name}</Link>{drives.length > 0 && <span className="muted small" title="Drives">{drives.length}</span>}</h2>
                  {vols.length > 0 && <SpaceGrid server={s} list={vols} />}
                  {drives.length > 0 && <DriveGrid list={drives} onOpen={pick} />}
                </section>
              ))}
            </div>
          )}
      </div>
      {importing && <ScrutinyImport onClose={() => setImporting(false)} />}
    </Shell>
  )
}
