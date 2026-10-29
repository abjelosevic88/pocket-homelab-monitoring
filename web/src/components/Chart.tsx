// Hand-drawn SVG charts: a time-series line chart with a crosshair tooltip,
// a sparkline and a meter. Colours come from CSS variables (--series-1..4,
// --st-*) so light and dark mode each get their own validated steps.
import React, { useLayoutEffect, useMemo, useRef, useState } from 'react'
import { Icon } from './icons'
import { type Level, LEVEL_ICON, LEVEL_LABEL } from '../lib/format'

export interface Series { key: string; label: string; values: (number | null)[] }

// ---------- scales ----------

/** Clean, rounded tick values from 0 to at least `max`. bytes → steps in 1024 units. */
export function niceTicks(max: number, count = 4, bytes = false): number[] {
  if (!(max > 0)) max = 1
  let unit = 1
  if (bytes) while (max / unit >= 1024) unit *= 1024
  const m = max / unit
  const raw = m / count
  const mag = Math.pow(10, Math.floor(Math.log10(raw)))
  const step = [1, 2, 2.5, 5, 10].map(f => f * mag).find(s => s >= raw) ?? 10 * mag
  const top = Math.ceil(m / step - 1e-9) * step
  const out: number[] = []
  for (let v = 0; v <= top + step / 2; v += step) out.push(+(v * unit).toPrecision(12))
  return out
}

const X_STEPS = [60, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400, 172800, 604800]
function timeTicks(from: number, to: number, want: number): { ticks: number[]; step: number } {
  const span = Math.max(1, to - from)
  const step = X_STEPS.find(s => span / s <= want) ?? X_STEPS[X_STEPS.length - 1]
  const tz = new Date().getTimezoneOffset() * 60 // align to local midnight/hour
  const first = Math.ceil((from - tz) / step) * step + tz
  const ticks: number[] = []
  for (let t = first; t <= to; t += step) ticks.push(t)
  return { ticks, step }
}
function fmtTick(t: number, step: number, h12: boolean): string {
  const d = new Date(t * 1000)
  if (step >= 86400) return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric' })
  return d.toLocaleTimeString('en-US', { hour: 'numeric', minute: '2-digit', hour12: h12 })
}
export function fmtWhen(t: number, h12: boolean, withDate: boolean): string {
  const d = new Date(t * 1000)
  const time = d.toLocaleTimeString('en-US', { hour: 'numeric', minute: '2-digit', second: withDate ? undefined : '2-digit', hour12: h12 })
  return withDate ? `${d.toLocaleDateString('en-US', { weekday: 'short', month: 'short', day: 'numeric' })}, ${time}` : time
}

function useWidth(ref: React.RefObject<HTMLElement | null>) {
  const [w, setW] = useState(0)
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    setW(el.clientWidth)
    const ro = new ResizeObserver(() => setW(el.clientWidth))
    ro.observe(el)
    return () => ro.disconnect()
  }, [ref])
  return w
}

// ---------- line chart ----------

export function LineChart({ xs, series, format, tickFormat, yMax, bytes, height = 170, loading, empty = 'No data for this range yet', h12 = false, step, label, area, guides = [] }: {
  xs: number[]
  series: Series[]
  /** Formats a value in the tooltip. */
  format: (v: number) => string
  /** Formats an axis tick (defaults to `format`). */
  tickFormat?: (v: number) => string
  /** Fixed top of the scale (100 for percentages); grows when data exceeds it. */
  yMax?: number
  bytes?: boolean
  height?: number
  loading?: boolean
  empty?: string
  h12?: boolean
  /** Expected seconds between points; a bigger gap breaks the line. */
  step?: number
  label: string
  area?: boolean
  /** Dashed reference lines (a plan, a limit), labelled at the right edge. */
  guides?: { value: number; label: string }[]
}) {
  const box = useRef<HTMLDivElement>(null)
  const width = useWidth(box)
  const [hover, setHover] = useState<number | null>(null)
  const tf = tickFormat || format
  const pad = { top: 8, right: 10, bottom: 22, left: 0 }

  const dataMax = useMemo(() => {
    let m = 0
    for (const s of series) for (const v of s.values) if (v !== null && v > m) m = v
    for (const g of guides) if (g.value > m) m = g.value
    return m
  }, [series, guides])
  const ticksY = useMemo(() => niceTicks(Math.max(yMax ?? 0, dataMax, yMax ? 0 : 1e-9) || 1, height < 140 ? 3 : 4, bytes), [yMax, dataMax, bytes, height])
  const top = ticksY[ticksY.length - 1] || 1
  // left gutter wide enough for the longest tick label
  const gutter = Math.max(28, Math.max(...ticksY.map(t => tf(t).length)) * 6.4 + 8)
  const x0 = pad.left + gutter, x1 = Math.max(x0 + 10, width - pad.right)
  const y0 = pad.top, y1 = height - pad.bottom
  const from = xs[0] ?? 0, to = xs[xs.length - 1] ?? 1
  const sx = (t: number) => x0 + ((t - from) / Math.max(1, to - from)) * (x1 - x0)
  const sy = (v: number) => y1 - (v / top) * (y1 - y0)
  const gap = step ? step * 2.5 : Infinity

  const paths = useMemo(() => series.map(s => {
    let d = '', a = '', run: [number, number][] = []
    const flush = () => {
      if (!run.length) return
      d += run.map(([x, y], i) => `${i ? 'L' : 'M'}${x.toFixed(1)},${y.toFixed(1)}`).join('')
      if (run.length > 1) a += `M${run[0][0].toFixed(1)},${y1}` + run.map(([x, y]) => `L${x.toFixed(1)},${y.toFixed(1)}`).join('') + `L${run[run.length - 1][0].toFixed(1)},${y1}Z`
      run = []
    }
    s.values.forEach((v, i) => {
      if (v === null || v === undefined || !isFinite(v)) { flush(); return }
      if (run.length && xs[i] - xs[i - 1] > gap) flush()
      run.push([sx(xs[i]), sy(v)])
    })
    flush()
    return { d, a }
  }), [series, xs, width, top, height]) // eslint-disable-line react-hooks/exhaustive-deps

  const { ticks: ticksX, step: xStep } = timeTicks(from, to, Math.max(2, Math.floor((x1 - x0) / 90)))
  const hasData = xs.length > 1 && series.some(s => s.values.some(v => v !== null))
  const multiDay = to - from > 86400

  const pick = (clientX: number) => {
    const r = box.current?.getBoundingClientRect()
    if (!r || !xs.length) return
    const t = from + ((clientX - r.left - x0) / Math.max(1, x1 - x0)) * (to - from)
    let lo = 0, hi = xs.length - 1
    while (hi - lo > 1) { const mid = (lo + hi) >> 1; if (xs[mid] < t) lo = mid; else hi = mid }
    setHover(Math.abs(xs[lo] - t) <= Math.abs(xs[hi] - t) ? lo : hi)
  }
  const onKey = (e: React.KeyboardEvent) => {
    if (!xs.length) return
    if (e.key === 'ArrowLeft') { e.preventDefault(); setHover(h => Math.max(0, (h ?? xs.length) - 1)) }
    else if (e.key === 'ArrowRight') { e.preventDefault(); setHover(h => Math.min(xs.length - 1, (h ?? -1) + 1)) }
    else if (e.key === 'Home') setHover(0)
    else if (e.key === 'End') setHover(xs.length - 1)
    else if (e.key === 'Escape') setHover(null)
  }
  const hx = hover !== null && xs[hover] !== undefined ? sx(xs[hover]) : null
  const tipLeft = hx !== null ? (hx > (x0 + x1) / 2 ? hx - 12 : hx + 12) : 0

  return (
    <div className={`viz ${loading ? 'refetching' : ''}`}>
      {series.length > 1 && (
        <ul className="viz-legend" aria-label="Legend">
          {series.map((s, i) => <li key={s.key}><span className="viz-key" style={{ background: `var(--series-${i + 1})` }} />{s.label}</li>)}
        </ul>
      )}
      <div ref={box} className="viz-plot" style={{ height }} tabIndex={hasData ? 0 : -1} role="img" aria-label={label}
        onPointerMove={e => pick(e.clientX)} onPointerLeave={() => setHover(null)} onKeyDown={onKey} onBlur={() => setHover(null)}>
        {width > 0 && (
          <svg width={width} height={height} aria-hidden="true">
            {ticksY.map(t => (
              <g key={t}>
                <line x1={x0} x2={x1} y1={sy(t)} y2={sy(t)} className={t === 0 ? 'viz-base' : 'viz-grid'} />
                <text x={x0 - 6} y={sy(t)} dy="0.32em" textAnchor="end" className="viz-tick">{tf(t)}</text>
              </g>
            ))}
            {hasData && guides.filter(g => g.value > 0).map(g => (
              <g key={g.label} className="viz-guide">
                <line x1={x0} x2={x1} y1={sy(g.value)} y2={sy(g.value)} />
                <text x={x1 - 4} y={sy(g.value) - 4} textAnchor="end">{g.label}</text>
              </g>
            ))}
            {hasData && ticksX.map(t => <text key={t} x={sx(t)} y={height - 5} textAnchor="middle" className="viz-tick">{fmtTick(t, xStep, h12)}</text>)}
            {hasData && paths.map((p, i) => (
              <g key={series[i].key}>
                {(area ?? series.length === 1) && <path d={p.a} fill={`var(--series-${i + 1})`} className="viz-area" />}
                <path d={p.d} fill="none" stroke={`var(--series-${i + 1})`} className="viz-line" />
              </g>
            ))}
            {hasData && hx !== null && (
              <g>
                <line x1={hx} x2={hx} y1={y0} y2={y1} className="viz-cross" />
                {series.map((s, i) => {
                  const v = s.values[hover!]
                  return v === null || v === undefined ? null : <circle key={s.key} cx={hx} cy={sy(v)} r={4} fill={`var(--series-${i + 1})`} className="viz-dot" />
                })}
              </g>
            )}
          </svg>
        )}
        {!hasData && <div className="viz-empty">{loading ? 'Loading…' : empty}</div>}
        {hasData && hover !== null && hx !== null && (
          <div className={`viz-tip ${hx > (x0 + x1) / 2 ? 'left' : ''}`} style={{ left: tipLeft }}>
            <div className="viz-tip-when">{fmtWhen(xs[hover], h12, multiDay)}</div>
            {series.map((s, i) => {
              const v = s.values[hover]
              return (
                <div key={s.key} className="viz-tip-row">
                  <span className="viz-key" style={{ background: `var(--series-${i + 1})` }} />
                  <b>{v === null || v === undefined ? '–' : format(v)}</b>
                  <span>{s.label}</span>
                </div>
              )
            })}
          </div>
        )}
      </div>
    </div>
  )
}

// ---------- sparkline ----------

export function Sparkline({ values, max = 100, width = 120, height = 32, label }: { values: number[]; max?: number; width?: number; height?: number; label?: string }) {
  if (values.length < 2) return <div className="spark spark-empty" style={{ width, height }} aria-hidden="true" />
  const top = Math.max(max, ...values) || 1
  const step = width / (values.length - 1)
  const pts = values.map((v, i) => [i * step, height - 2 - (Math.max(0, v) / top) * (height - 6)] as const)
  const d = pts.map(([x, y], i) => `${i ? 'L' : 'M'}${x.toFixed(1)},${y.toFixed(1)}`).join('')
  const last = pts[pts.length - 1]
  return (
    <svg className="spark" width={width} height={height} role="img" aria-label={label}>
      <path d={`${d}L${width},${height}L0,${height}Z`} className="spark-area" />
      <path d={d} className="spark-line" />
      <circle cx={last[0] - 1} cy={last[1]} r={3} className="spark-dot" />
    </svg>
  )
}

// ---------- meter ----------

/** A horizontal bar whose fill carries severity; warn/crit also get an icon + label. */
export function Meter({ value, lvl, label, thin, showLevel = true }: { value: number; lvl: Level; label?: string; thin?: boolean; showLevel?: boolean }) {
  const pct = Math.max(0, Math.min(100, value || 0))
  return (
    <span className={`meter ${lvl} ${thin ? 'thin' : ''}`}>
      <span className="meter-track" role="meter" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(pct)} aria-label={label}>
        <span className="meter-fill" style={{ width: `${pct}%` }} />
      </span>
      {showLevel && lvl !== 'ok' && <span className="meter-flag"><Icon name={LEVEL_ICON[lvl]} size={12} />{LEVEL_LABEL[lvl]}</span>}
    </span>
  )
}

/** A badge that states a level with an icon (never colour alone). */
export function LevelBadge({ lvl, children }: { lvl: Level; children?: React.ReactNode }) {
  return <span className={`lvl-badge ${lvl}`}><Icon name={LEVEL_ICON[lvl]} size={12} />{children ?? LEVEL_LABEL[lvl]}</span>
}
