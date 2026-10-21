// Numbers for people: bytes, rates, percentages, durations, temperatures.

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']

/** 1536 → "1.5 KB" (binary steps, short labels). */
export function fmtBytes(n: number | null | undefined, digits = 1): string {
  if (n === null || n === undefined || !isFinite(n)) return '–'
  let v = Math.abs(n), i = 0
  while (v >= 1024 && i < UNITS.length - 1) { v /= 1024; i++ }
  const d = i === 0 ? 0 : v >= 100 ? 0 : v >= 10 ? Math.min(1, digits) : digits
  return `${n < 0 ? '-' : ''}${v.toFixed(d)} ${UNITS[i]}`
}

/** Bytes per second → "1.2 MB/s". */
export function fmtRate(n: number | null | undefined): string {
  if (n === null || n === undefined || !isFinite(n)) return '–'
  if (n < 1) return '0 B/s'
  return `${fmtBytes(n)}/s`
}

export function fmtPct(n: number | null | undefined, digits = 0): string {
  if (n === null || n === undefined || !isFinite(n)) return '–'
  return `${n.toFixed(n > 0 && n < 1 && digits === 0 ? 1 : digits)}%`
}

export function fmtNum(n: number | null | undefined, digits = 0): string {
  if (n === null || n === undefined || !isFinite(n)) return '–'
  if (Math.abs(n) >= 1e6) return `${(n / 1e6).toFixed(1)}M`
  if (Math.abs(n) >= 1e4) return `${(n / 1e3).toFixed(1)}K`
  return n.toLocaleString('en-US', { maximumFractionDigits: digits, minimumFractionDigits: digits })
}

/** Seconds → "3d 4h", "5h 12m", "42m", "18s". */
export function fmtDuration(sec: number | null | undefined): string {
  if (sec === null || sec === undefined || !isFinite(sec) || sec < 0) return '–'
  const s = Math.floor(sec)
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60)
  if (d > 0) return h ? `${d}d ${h}h` : `${d}d`
  if (h > 0) return m ? `${h}h ${m}m` : `${h}h`
  if (m > 0) return `${m}m`
  return `${s}s`
}

/** ISO or unix seconds → "just now", "3 min ago", "2 h ago", "yesterday", "Oct 12". */
export function ago(t: string | number | null | undefined): string {
  if (t === null || t === undefined || t === '' || t === 0) return 'never'
  const ms = typeof t === 'number' ? t * 1000 : Date.parse(t)
  if (isNaN(ms) || ms <= 0) return 'never'
  const s = Math.round((Date.now() - ms) / 1000)
  if (s < 45) return 'just now'
  if (s < 3600) return `${Math.max(1, Math.round(s / 60))} min ago`
  if (s < 86400) return `${Math.round(s / 3600)} h ago`
  if (s < 2 * 86400) return 'yesterday'
  if (s < 14 * 86400) return `${Math.round(s / 86400)} days ago`
  return new Date(ms).toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: new Date(ms).getFullYear() === new Date().getFullYear() ? undefined : 'numeric' })
}

/** Seconds between two ISO times (to = now when empty). */
export function secondsBetween(from: string, to?: string | null): number {
  const a = Date.parse(from), b = to ? Date.parse(to) : Date.now()
  return Math.max(0, (b - a) / 1000)
}

export type TempUnit = 'C' | 'F'
export const toUnit = (c: number, u: TempUnit) => (u === 'F' ? c * 9 / 5 + 32 : c)
export function fmtTemp(c: number | null | undefined, u: TempUnit = 'C', digits = 0): string {
  if (c === null || c === undefined || !isFinite(c) || c <= 0) return '–'
  return `${toUnit(c, u).toFixed(digits)}°${u}`
}

/** A date + time, "Oct 21, 14:05". */
export function fmtDateTime(t: string | number, h12 = false): string {
  const d = typeof t === 'number' ? new Date(t * 1000) : new Date(t)
  if (isNaN(d.getTime())) return '–'
  return d.toLocaleString('en-US', { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit', hour12: h12 })
}

/** Severity of a percentage for meters: ok < 70 ≤ warn < 90 ≤ crit. */
export type Level = 'ok' | 'warn' | 'crit'
export function level(pct: number, warn = 70, crit = 90): Level {
  return pct >= crit ? 'crit' : pct >= warn ? 'warn' : 'ok'
}
export const LEVEL_LABEL: Record<Level, string> = { ok: 'OK', warn: 'High', crit: 'Critical' }
export const LEVEL_ICON: Record<Level, string> = { ok: 'check-circle', warn: 'alert', crit: 'alert' }

/** Temperature severity: near the sensor's own high/critical, else 75/90 °C. */
export function tempLevel(c: number, high?: number, critical?: number): Level {
  const crit = critical && critical > 20 ? critical - 5 : 90
  const warn = high && high > 20 ? high : Math.min(75, crit - 10)
  return c >= crit ? 'crit' : c >= warn ? 'warn' : 'ok'
}

export function plural(n: number, one: string, many = one + 's'): string { return `${n} ${n === 1 ? one : many}` }
