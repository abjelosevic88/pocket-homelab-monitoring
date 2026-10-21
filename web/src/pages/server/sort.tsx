import { useMemo, useState } from 'react'
import { Icon } from '../../components/icons'

export interface SortState<T> { rows: T[]; key: keyof T; dir: 'asc' | 'desc'; set: (k: keyof T) => void }

/** Client-side sorting for small tables; clicking the active column flips direction. */
export function useSort<T>(rows: T[], initial: keyof T, initialDir: 'asc' | 'desc' = 'desc'): SortState<T> {
  const [key, setKey] = useState<keyof T>(initial)
  const [dir, setDir] = useState<'asc' | 'desc'>(initialDir)
  const sorted = useMemo(() => {
    const out = [...rows]
    out.sort((a, b) => {
      const x = a[key] as unknown, y = b[key] as unknown
      const c = typeof x === 'number' && typeof y === 'number' ? x - y : String(x ?? '').localeCompare(String(y ?? ''), undefined, { numeric: true })
      return dir === 'asc' ? c : -c
    })
    return out
  }, [rows, key, dir])
  const set = (k: keyof T) => {
    if (k === key) setDir(d => (d === 'asc' ? 'desc' : 'asc'))
    else { setKey(k); setDir(typeof rows[0]?.[k] === 'number' ? 'desc' : 'asc') }
  }
  return { rows: sorted, key, dir, set }
}

export function SortTh<T>({ sort, k, label, right, className = '' }: { sort: SortState<T>; k: keyof T; label: string; right?: boolean; className?: string }) {
  const on = sort.key === k
  return (
    <th className={`${right ? 'right' : ''} ${className}`} aria-sort={on ? (sort.dir === 'asc' ? 'ascending' : 'descending') : 'none'}>
      <button type="button" className={`th-sort ${on ? 'on' : ''}`} onClick={() => sort.set(k)}>
        {label}{on && <Icon name={sort.dir === 'asc' ? 'chevron-up' : 'chevron-down'} size={12} strokeWidth={2.2} />}
      </button>
    </th>
  )
}
