import { Icon } from './icons'

// ---------- colours ----------

export const PALETTE = ['#3b82f6', '#6366f1', '#8b5cf6', '#d946ef', '#ec4899', '#ef4444', '#f97316', '#f59e0b', '#eab308', '#84cc16', '#10b981', '#14b8a6', '#06b6d4', '#64748b']

export function ColorPalette({ value, onChange, allowNone, noneLabel = 'No colour', custom = true }: { value: string; onChange: (c: string) => void; allowNone?: boolean; noneLabel?: string; custom?: boolean }) {
  const lower = (value || '').toLowerCase()
  const isCustom = !!value && !PALETTE.includes(lower)
  return (
    <div className="palette" role="radiogroup" aria-label="Colour">
      {allowNone && <button type="button" role="radio" aria-checked={!value} className={`swatch none ${!value ? 'active' : ''}`} title={noneLabel} aria-label={noneLabel} onClick={() => onChange('')}><Icon name="x" size={12} /></button>}
      {PALETTE.map(c => <button key={c} type="button" role="radio" aria-checked={lower === c} aria-label={c} className={`swatch ${lower === c ? 'active' : ''}`} style={{ background: c }} onClick={() => onChange(c)} />)}
      {custom && (
        <label className={`swatch custom ${isCustom ? 'active' : ''}`} title="Custom colour" style={isCustom ? { background: value } : undefined}>
          {!isCustom && <Icon name="plus" size={12} />}
          <input type="color" value={value || '#3b82f6'} onChange={e => onChange(e.target.value)} aria-label="Custom colour" />
        </label>
      )}
    </div>
  )
}
