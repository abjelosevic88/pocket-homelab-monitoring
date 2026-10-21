// The history grid: CPU, memory, network, disk I/O, temperatures, load.
import { useMemo } from 'react'
import { useApp } from '../../lib/app-context'
import { fmtPct, fmtRate, toUnit } from '../../lib/format'
import type { Point, RangeName } from '../../lib/types'
import { LineChart, type Series } from '../../components/Chart'
import { Icon } from '../../components/icons'

export interface ChartData { points: Point[]; step: number; sensors: { key: string; label: string }[] }

const MAX_SERIES = 4

function ChartCard({ title, icon, now, children }: { title: string; icon: string; now?: string; children: React.ReactNode }) {
  return (
    <section className="chart-card">
      <header className="chart-head">
        <Icon name={icon} size={15} className="muted" />
        <h3>{title}</h3>
        <span className="spacer" />
        {now && <span className="chart-now tnum">{now}</span>}
      </header>
      {children}
    </section>
  )
}

export function HistoryCharts({ data, range, loading }: { data: ChartData | null; range: RangeName; loading: boolean }) {
  const { settings, h12 } = useApp()
  const unit = settings?.temp_unit || 'C'
  const pts = data?.points || []
  const xs = useMemo(() => pts.map(p => p.t), [pts])
  const col = (f: (p: Point) => number | null | undefined) => pts.map(p => { const v = f(p); return v === undefined || v === null || !isFinite(v) ? null : v })
  const last = pts[pts.length - 1]
  const step = data?.step || 0
  const common = { xs, h12, step, loading, empty: range === 'live' ? 'Collecting — the first points arrive in a few seconds' : 'No history for this range yet' }

  const temps = useMemo<Series[]>(() => {
    if (!pts.length) return []
    const sensors = data?.sensors || []
    if (sensors.length && pts.some(p => p.temps)) {
      // the hottest few sensors keep their own line; the rest stay in the Sensors panel
      const peak = (k: string) => Math.max(0, ...pts.map(p => p.temps?.[k] ?? 0))
      return [...sensors].sort((a, b) => peak(b.key) - peak(a.key)).slice(0, MAX_SERIES)
        .map(s => ({ key: s.key, label: s.label, values: pts.map(p => p.temps?.[s.key] !== undefined ? toUnit(p.temps![s.key], unit) : null) }))
    }
    return [{ key: 'temp', label: 'Hottest sensor', values: pts.map(p => p.temp > 0 ? toUnit(p.temp, unit) : null) }]
  }, [pts, data?.sensors, unit])
  const hiddenSensors = Math.max(0, (data?.sensors.length || 0) - MAX_SERIES)
  const tempsAny = temps.some(s => s.values.some(v => v !== null))

  const cpu: Series[] = [{ key: 'cpu', label: 'Total', values: col(p => p.cpu) }, { key: 'iowait', label: 'Waiting on disk (iowait)', values: col(p => p.iowait) }]
  const mem: Series[] = [{ key: 'mem', label: 'Memory', values: col(p => p.mem) }, { key: 'swap', label: 'Swap', values: col(p => p.swap) }]
  const net: Series[] = [{ key: 'rx', label: 'Received', values: col(p => p.rx) }, { key: 'tx', label: 'Sent', values: col(p => p.tx) }]
  const disk: Series[] = [{ key: 'read', label: 'Read', values: col(p => p.read) }, { key: 'write', label: 'Write', values: col(p => p.write) }]
  const load: Series[] = [{ key: 'l1', label: '1 minute', values: col(p => p.load1) }, { key: 'l5', label: '5 minutes', values: col(p => p.load5) }, { key: 'l15', label: '15 minutes', values: col(p => p.load15) }]
  const degree = (v: number) => `${Math.round(v)}°`

  return (
    <div className="chart-grid">
      <ChartCard title="CPU" icon="cpu" now={last ? fmtPct(last.cpu) : undefined}>
        <LineChart {...common} series={cpu} yMax={100} format={v => fmtPct(v, 1)} tickFormat={v => `${v}%`} label="CPU use over time" area={false} />
      </ChartCard>
      <ChartCard title="Memory" icon="memory" now={last ? fmtPct(last.mem) : undefined}>
        <LineChart {...common} series={mem} yMax={100} format={v => fmtPct(v, 1)} tickFormat={v => `${v}%`} label="Memory and swap use over time" />
      </ChartCard>
      <ChartCard title="Network" icon="network" now={last ? `↓ ${fmtRate(last.rx)}  ↑ ${fmtRate(last.tx)}` : undefined}>
        <LineChart {...common} series={net} bytes format={fmtRate} tickFormat={v => fmtRate(v).replace('/s', '')} label="Network traffic over time" />
      </ChartCard>
      <ChartCard title="Disk I/O" icon="disk" now={last ? `R ${fmtRate(last.read)}  W ${fmtRate(last.write)}` : undefined}>
        <LineChart {...common} series={disk} bytes format={fmtRate} tickFormat={v => fmtRate(v).replace('/s', '')} label="Disk reads and writes over time" />
      </ChartCard>
      {(tempsAny || !pts.length) && (
        <ChartCard title="Temperatures" icon="thermometer" now={last && last.temp > 0 ? `${Math.round(toUnit(last.temp, unit))}°${unit}` : undefined}>
          <LineChart {...common} series={temps} format={v => `${v.toFixed(1)}°${unit}`} tickFormat={degree} yMax={unit === 'F' ? 120 : 50} label="Temperatures over time" area={false} />
          {hiddenSensors > 0 && <p className="chart-note muted xsmall">The {MAX_SERIES} hottest sensors are drawn; {hiddenSensors} more are in the Sensors panel.</p>}
        </ChartCard>
      )}
      <ChartCard title="Load" icon="gauge" now={last ? last.load1.toFixed(2) : undefined}>
        <LineChart {...common} series={load} format={v => v.toFixed(2)} tickFormat={v => String(+v.toFixed(2))} label="Load average over time" />
      </ChartCard>
    </div>
  )
}
