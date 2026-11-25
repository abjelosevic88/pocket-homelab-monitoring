// Free space on the Drives page: a server's file systems folded into the
// volumes a person thinks in — one card per disk or ZFS pool, not per dataset.
import { Link } from 'react-router-dom'
import { Meter } from '../../components/Chart'
import { fmtBytes, fmtPct, level } from '../../lib/format'
import type { FS, Server } from '../../lib/types'

export interface Volume { key: string; name: string; detail: string; total: number; used: number; free: number; percent: number }

const MIN_SIZE = 4 * 1024 ** 3 // /boot, EFI and ZFS's small system datasets are not where things fill up

/** Network shares are skipped (they show on the machine that has the disks); ZFS datasets
 *  share their pool's free space, so they fold into one volume per pool, and so do btrfs subvolumes. */
export function volumes(fs: FS[]): Volume[] {
  const by = new Map<string, { mounts: FS[]; zfs: boolean }>()
  for (const f of fs) {
    if (f.network || !f.total) continue
    const zfs = f.type === 'zfs'
    const key = zfs ? 'zfs:' + f.device.split('/')[0] : f.type === 'btrfs' ? `btrfs:${f.device}` : `dev:${f.device}:${f.mount}`
    const g = by.get(key) || { mounts: [], zfs }
    g.mounts.push(f)
    by.set(key, g)
  }
  const out: Volume[] = []
  for (const [key, { mounts, zfs }] of by) {
    const top = [...mounts].sort((a, b) => a.mount.length - b.mount.length)[0]
    const free = Math.max(...mounts.map(m => m.free))
    // a dataset's df size is its own data plus the pool's free space, so the pool is the sum of the data plus that free
    const used = zfs ? mounts.reduce((n, m) => n + m.used, 0) : top.used
    const total = zfs ? used + free : top.total
    if (total < MIN_SIZE) continue
    const name = zfs ? top.device.split('/')[0] : top.mount
    const detail = zfs ? `ZFS pool · ${mounts.length} dataset${mounts.length === 1 ? '' : 's'}` : `${top.device.replace(/^\/dev\/(mapper\/)?/, '')} · ${top.type}`
    out.push({ key, name, detail, total, used, free, percent: used + free > 0 ? (used / (used + free)) * 100 : 0 })
  }
  return out.sort((a, b) => b.total - a.total)
}

export function SpaceGrid({ server, list }: { server: Server; list: Volume[] }) {
  return (
    <div className="space-grid">
      {list.map(v => {
        const lvl = level(v.percent, 80, 90)
        return (
          <Link key={v.key} to={`/server/${server.id}?tab=io`} className={`space-card ${lvl}`} title={`${v.name}: ${fmtBytes(v.used)} used, ${fmtBytes(v.free)} free of ${fmtBytes(v.total)}`}>
            <div className="space-head">
              <span className="mono strong ellipsis">{v.name}</span>
              <span className="muted xsmall ellipsis">{v.detail}</span>
            </div>
            <Meter value={v.percent} lvl={lvl} label={`${v.name} used`} thin showLevel={false} />
            <div className="space-nums">
              <span><span className="strong tnum">{fmtBytes(v.free)}</span> <span className="muted">free</span></span>
              <span className="muted tnum">{fmtPct(v.percent)} of {fmtBytes(v.total, 0)}</span>
            </div>
          </Link>
        )
      })}
    </div>
  )
}
