import type { Server, ServerStatus } from '../lib/types'
import { Icon } from './icons'

export const STATUS_LABEL: Record<ServerStatus, string> = { up: 'Online', down: 'Offline', paused: 'Paused', unknown: 'Waiting for data' }

/** Server state as a shape + colour + label (never colour alone). */
export function ServerState({ status, label, size = 'sm' }: { status: ServerStatus; label?: boolean; size?: 'sm' | 'md' }) {
  const icon = status === 'down' ? <Icon name="alert" size={size === 'md' ? 14 : 12} strokeWidth={2.2} />
    : status === 'paused' ? <Icon name="pause" size={size === 'md' ? 12 : 10} strokeWidth={2.4} />
    : <span className="st-dot" />
  return (
    <span className={`st st-${status} ${size}`} title={STATUS_LABEL[status]} aria-label={label ? undefined : STATUS_LABEL[status]} role={label ? undefined : 'img'}>
      {icon}{label && <span className="st-label">{STATUS_LABEL[status]}</span>}
    </span>
  )
}

export function serverSubtitle(s: Server): string {
  const h = s.summary
  return [h?.hostname && h.hostname !== s.name ? h.hostname : '', h?.platform].filter(Boolean).join(' · ') || (s.kind === 'local' ? 'This server' : s.url)
}
