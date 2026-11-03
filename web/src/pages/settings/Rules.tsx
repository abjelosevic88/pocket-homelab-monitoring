// Alert rules of one of my servers.
import { useEffect, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { api, V1 } from '../../lib/api'
import { useApp } from '../../lib/app-context'
import { fmtDuration, toUnit } from '../../lib/format'
import type { Metric, Rule, Snapshot } from '../../lib/types'
import { Icon } from '../../components/icons'
import { type Option, Select } from '../../components/Select'
import { Confirm, Empty, errMsg, Field, Loading, MenuButton, Modal, Segmented, Switch, useAsync, useToast } from '../../components/ui'
import { METRIC_ICON } from '../Alerts'
import { Section } from './common'

export const METRICS: { value: Metric; label: string; help: string; unit: string; def: number; target?: 'mount' | 'sensor' | 'container' | 'drive'; noThreshold?: boolean; below?: boolean; tests?: boolean }[] = [
  { value: 'cpu', label: 'CPU', help: 'Whole-machine CPU use.', unit: '%', def: 90 },
  { value: 'mem', label: 'Memory', help: 'Memory in use (cache that can be freed doesn’t count).', unit: '%', def: 90 },
  { value: 'swap', label: 'Swap', help: 'Swap space in use.', unit: '%', def: 80 },
  { value: 'load', label: 'Load', help: 'The 1-minute load per thread: 1.0 means every thread is busy.', unit: '× threads', def: 1.5 },
  { value: 'disk', label: 'Disk space', help: 'How full a file system is.', unit: '%', def: 90, target: 'mount' },
  { value: 'temp', label: 'Temperature', help: 'A temperature sensor.', unit: '°', def: 90, target: 'sensor' },
  { value: 'offline', label: 'Offline', help: 'No reading arrived from the server.', unit: '', def: 0, noThreshold: true },
  { value: 'container', label: 'Container', help: 'A container turned unhealthy, keeps restarting, or crashed (stopped with an error; a deliberate stop is fine).', unit: '', def: 0, target: 'container', noThreshold: true },
  { value: 'raid', label: 'RAID', help: 'A software RAID array is degraded.', unit: '', def: 0, noThreshold: true },
  { value: 'job', label: 'Scheduled jobs', help: 'A scheduled job (systemd timer) failed, has been running too long, or stopped being scheduled. Mute one job from its menu on the Jobs tab.', unit: '', def: 0, noThreshold: true },
  { value: 'speedtest', label: 'Speed test failed', help: 'The internet speed test couldn’t run: the connection is down or the test server didn’t answer. A failed scheduled test is tried again 10 minutes later.', unit: '', def: 0, noThreshold: true, tests: true },
  { value: 'speed_down', label: 'Download speed', help: 'The internet download speed measured by the speed test.', unit: 'Mbps', def: 50, below: true, tests: true },
  { value: 'speed_up', label: 'Upload speed', help: 'The internet upload speed measured by the speed test.', unit: 'Mbps', def: 10, below: true, tests: true },
  { value: 'speed_ping', label: 'Ping', help: 'The idle ping to the speed test server.', unit: 'ms', def: 50, tests: true },
  { value: 'check', label: 'Uptime check down', help: 'A website, port, DNS or push check on the Uptime tab is down (after its own retries). Mute one check from its menu there.', unit: '', def: 0, noThreshold: true },
  { value: 'drive', label: 'Drive failing', help: 'A drive’s SMART says it is failing — its own health check, an attribute at the maker’s failure threshold, spare space used up, a failed self-test — or a drive stopped answering for an hour (a dead disk often just disappears).', unit: '', def: 0, target: 'drive', noThreshold: true },
  { value: 'drive_errors', label: 'Drive bad sectors', help: 'Reallocated, pending or uncorrectable sectors (NVMe: media errors) above this count. Any at all is an early warning; set it to today’s count to hear only about new ones.', unit: 'sectors', def: 0, target: 'drive' },
  { value: 'drive_wear', label: 'Drive wear', help: 'An SSD has used this much of its rated write endurance.', unit: '%', def: 80, target: 'drive' },
  { value: 'drive_temp', label: 'Drive temperature', help: 'A drive runs hotter than this. 0 uses each drive’s own limit: an NVMe drive’s warning temperature, 55 °C for spinning disks, 70 °C for SATA SSDs.', unit: '°', def: 0, target: 'drive' },
  { value: 'cert', label: 'Certificate expiring', help: 'The HTTPS certificate of a website check expires within this many days.', unit: 'days', def: 14, below: true },
]
const TESTS: Option<number>[] = [1, 2, 3, 4, 5].map(n => ({ value: n, label: n === 1 ? 'After one test' : `${n} tests in a row` }))
const FOR: Option<number>[] = [0, 60, 120, 300, 600, 900, 1800, 3600].map(s => ({ value: s, label: s === 0 ? 'Right away' : `For ${fmtDuration(s).replace('m', ' min').replace('h', ' h')}` }))

export function RulesTab() {
  const { servers, settings } = useApp()
  const [search, setSearch] = useSearchParams()
  const { toast } = useToast()
  const mine = servers.filter(s => s.role === 'owner')
  const sid = Number(search.get('server')) || mine[0]?.id || 0
  const server = mine.find(s => s.id === sid)
  const rules = useAsync(() => sid ? api.get<Rule[]>(`${V1}/rules?server_id=${sid}`) : Promise.resolve([] as Rule[]), [sid])
  const [edit, setEdit] = useState<Partial<Rule> | null>(null)
  const [del, setDel] = useState<Rule | null>(null)
  const unit = settings?.temp_unit || 'C'

  if (!mine.length) return <Empty title="No servers of your own" icon="bell">Alert rules belong to the servers you added. Shared servers follow their owner’s rules.</Empty>

  const patch = async (r: Rule, p: Partial<Rule>) => {
    rules.setData(list => (list || []).map(x => x.id === r.id ? { ...x, ...p } : x))
    try { await api.put(`${V1}/rules/${r.id}`, p) } catch (e) { toast(errMsg(e), 'err'); rules.reload() }
  }
  const describe = (r: Rule) => {
    const m = METRICS.find(x => x.value === r.metric)
    const dur = r.for_s ? ` for ${fmtDuration(r.for_s)}` : ''
    switch (r.metric) {
      case 'offline': return `No reading for ${fmtDuration(Math.max(r.for_s, 30))}`
      case 'container': return `${r.target ? `“${r.target}”` : 'Any container'} unhealthy or crashed${dur}`
      case 'raid': return `An array is degraded${dur}`
      case 'temp': return `${r.target ? r.target.split('/').pop() : 'Any sensor'} above ${Math.round(toUnit(r.threshold, unit))}°${unit}${dur}`
      case 'disk': return `${r.target || 'Any file system'} above ${r.threshold}%${dur}`
      case 'load': return `Load above ${r.threshold} per thread${dur}`
      case 'speedtest': return r.for_s > 1 ? `${r.for_s} failed tests in a row` : 'A test fails'
      case 'speed_down': case 'speed_up': return `${m?.label} under ${r.threshold} Mbps${r.for_s > 1 ? ` in ${r.for_s} tests in a row` : ''}`
      case 'speed_ping': return `Ping over ${r.threshold} ms${r.for_s > 1 ? ` in ${r.for_s} tests in a row` : ''}`
      case 'check': return `${r.target ? 'One check' : 'Any uptime check'} goes down${dur}`
      case 'cert': return `A certificate expires within ${r.threshold} days`
      case 'drive': return `${r.target ? 'This drive' : 'Any drive'} failing or gone`
      case 'drive_errors': return `${r.target ? 'This drive' : 'Any drive'} with more than ${r.threshold} bad sector${r.threshold === 1 ? '' : 's'}`
      case 'drive_wear': return `${r.target ? 'This drive' : 'Any drive'} over ${r.threshold}% worn`
      case 'drive_temp': return `${r.target ? 'This drive' : 'Any drive'} above ${r.threshold ? `${Math.round(toUnit(r.threshold, unit))}°${unit}` : 'its own limit'}${dur}`
      default: return `${m?.label} above ${r.threshold}%${dur}`
    }
  }

  return (
    <div className="stack loose">
      <Section title="Server" sub="Rules belong to each server. New servers start with a sensible set.">
        <Select value={sid} onChange={v => setSearch({ server: String(v) }, { replace: true })} label="Server" options={mine.map(s => ({ value: s.id, label: s.name, color: s.color }))} />
      </Section>
      <Section title={`Rules for ${server?.name || ''}`} actions={<button className="btn primary sm" onClick={() => setEdit({ server_id: sid, metric: 'cpu', threshold: 90, for_s: 300, severity: 'warning', enabled: true, target: '' })}><Icon name="plus" size={14} />Add rule</button>}>
        {!rules.data ? <Loading /> : !rules.data.length ? <Empty title="No rules" icon="bell">This server never raises an alert.</Empty> : (
          <div className="set-cal-list">
            {rules.data.map(r => (
              <div key={r.id} className={`set-cal-row ${r.enabled ? '' : 'dim'}`}>
                <span className="rule-ico"><Icon name={METRIC_ICON[r.metric] || 'bell'} size={15} /></span>
                <div className="grow">
                  <div className="set-cal-name">{METRICS.find(m => m.value === r.metric)?.label}<span className={`lvl-badge sm ${r.severity === 'critical' ? 'crit' : 'warn'}`}><Icon name="alert" size={11} />{r.severity === 'critical' ? 'Critical' : 'Warning'}</span></div>
                  <div className="set-cal-meta">{describe(r)}</div>
                </div>
                <Switch label={r.enabled ? 'Turn off' : 'Turn on'} checked={r.enabled} onChange={v => patch(r, { enabled: v })} />
                <MenuButton label="Rule options" align="right" items={[
                  { label: 'Edit…', icon: 'edit', onSelect: () => setEdit(r) },
                  { label: 'Delete', icon: 'trash', danger: true, onSelect: () => setDel(r) },
                ]}><Icon name="more" size={16} /></MenuButton>
              </div>
            ))}
          </div>
        )}
      </Section>
      <p className="muted small">Alerts go where Settings → Notifications says, once when they start and (if you like) once when they clear.</p>
      {edit && <RuleDialog rule={edit} onClose={() => setEdit(null)} onSaved={() => { setEdit(null); rules.reload() }} />}
      {del && <Confirm title="Delete this rule?" message={describe(del)} onCancel={() => setDel(null)}
        onConfirm={async () => { const d = del; setDel(null); try { await api.del(`${V1}/rules/${d.id}`); rules.reload() } catch (e) { toast(errMsg(e), 'err') } }} />}
    </div>
  )
}

function RuleDialog({ rule, onClose, onSaved }: { rule: Partial<Rule>; onClose: () => void; onSaved: () => void }) {
  const { settings } = useApp()
  const { toast } = useToast()
  const unit = settings?.temp_unit || 'C'
  const isNew = !rule.id
  const [f, setF] = useState({ metric: rule.metric || 'cpu', target: rule.target || '', threshold: rule.threshold ?? 90, for_s: rule.for_s ?? 300, severity: rule.severity || 'warning', enabled: rule.enabled ?? true })
  const m = METRICS.find(x => x.value === f.metric)!
  // temperatures are stored in °C and typed in the person's unit
  const isTemp = (x: string) => x === 'temp' || x === 'drive_temp'
  const showTemp = (v: number) => v === 0 && f.metric === 'drive_temp' ? 0 : Math.round(toUnit(v, unit))
  const [shown, setShown] = useState(() => String(isTemp(f.metric) ? showTemp(f.threshold) : f.threshold))
  const snap = useAsync(() => api.get<Snapshot>(`${V1}/servers/${rule.server_id}/snapshot`).catch(() => null), [rule.server_id])
  const [busy, setBusy] = useState(false)
  useEffect(() => { if (!rule.id || f.metric !== rule.metric) { setF(x => ({ ...x, threshold: m.def, target: '', for_s: m.tests ? 2 : x.for_s > 10 || x.for_s === 0 ? x.for_s : 300 })); setShown(String(isTemp(m.value) ? (m.def === 0 ? 0 : Math.round(toUnit(m.def, unit))) : m.def)) } }, [f.metric]) // eslint-disable-line react-hooks/exhaustive-deps

  const targets: Option<string>[] = (() => {
    const s = snap.data
    const any = { value: '', label: m.target === 'mount' ? 'Any file system' : m.target === 'sensor' ? 'Any sensor' : m.target === 'drive' ? 'Any drive' : 'Any container' }
    if (!s || !m.target) return [any]
    if (m.target === 'drive') return [any, ...(s.drives || []).map(x => ({ value: x.key, label: `${x.model} (${x.name.replace('/dev/', '')})`, description: x.serial }))]
    if (m.target === 'mount') return [any, ...s.fs.map(x => ({ value: x.mount, label: x.mount, description: `${x.device} · ${x.type}` }))]
    if (m.target === 'sensor') return [any, ...s.sensors.filter(x => x.kind === 'temp').map(x => ({ value: x.key, label: x.label, description: x.chip }))]
    return [any, ...(s.containers || []).map(x => ({ value: x.name, label: x.name, description: x.image }))]
  })()
  if (f.target && !targets.some(t => t.value === f.target)) targets.push({ value: f.target, label: f.target })

  const save = async () => {
    const n = parseFloat(shown.replace(',', '.'))
    if (!m.noThreshold && !isFinite(n)) { toast('Enter a number', 'err'); return }
    const threshold = m.noThreshold ? 0 : isTemp(f.metric) && !(f.metric === 'drive_temp' && n === 0) ? (unit === 'F' ? (n - 32) * 5 / 9 : n) : n
    setBusy(true)
    try {
      const body = { ...f, threshold, server_id: rule.server_id }
      if (isNew) await api.post(`${V1}/rules`, body); else await api.put(`${V1}/rules/${rule.id}`, body)
      onSaved()
    } catch (e) { toast(errMsg(e), 'err') } finally { setBusy(false) }
  }
  const forOpts = m.tests ? TESTS : f.metric === 'offline' ? FOR.filter(o => o.value >= 60) : FOR
  return (
    <Modal title={isNew ? 'Add an alert rule' : 'Edit alert rule'} onClose={onClose} footer={<>
      <button className="btn" onClick={onClose}>Cancel</button>
      <button className="btn primary" disabled={busy} onClick={save}>{isNew ? 'Add rule' : 'Save'}</button>
    </>}>
      <div className="stack">
        <Field label="Watch" group help={m.help}>
          <Select value={f.metric} onChange={v => setF({ ...f, metric: v })} label="Metric" options={METRICS.map(x => ({ value: x.value, label: x.label, icon: <Icon name={METRIC_ICON[x.value]} size={14} /> }))} />
        </Field>
        {m.target && <Field label={m.target === 'mount' ? 'File system' : m.target === 'sensor' ? 'Sensor' : m.target === 'drive' ? 'Drive' : 'Container'} group>
          <Select value={f.target} onChange={v => setF({ ...f, target: v })} options={targets} label="Target" searchable />
        </Field>}
        <div className="two-col">
          {!m.noThreshold && (
            <Field label={m.below ? 'Below' : 'Above'} help={isTemp(f.metric) ? `°${unit}${f.metric === 'drive_temp' ? ' · 0 = the drive’s own limit' : ''}` : m.unit === '%' ? 'percent' : m.unit}>
              <input value={shown} onChange={e => setShown(e.target.value)} inputMode="decimal" />
            </Field>
          )}
          <Field label={f.metric === 'offline' ? 'After' : m.tests ? 'When' : 'For at least'} group>
            <Select value={forOpts.some(o => o.value === f.for_s) ? f.for_s : forOpts[0].value} onChange={v => setF({ ...f, for_s: v })} options={forOpts} label="Duration" />
          </Field>
        </div>
        <Field label="Severity" group>
          <Segmented label="Severity" value={f.severity} onChange={v => setF({ ...f, severity: v })} options={[
            { value: 'warning', label: <span className="row"><Icon name="alert" size={13} />Warning</span> },
            { value: 'critical', label: <span className="row"><Icon name="alert" size={13} />Critical</span> },
          ]} />
        </Field>
      </div>
    </Modal>
  )
}
