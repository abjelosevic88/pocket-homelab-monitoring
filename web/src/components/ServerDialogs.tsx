// Add / edit a server, share it, and the actions every server menu offers.
import React, { useCallback, useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { api, V1 } from '../lib/api'
import { useApp } from '../lib/app-context'
import type { Person, Server, ServerKind, Share } from '../lib/types'
import { Icon } from './icons'
import { ColorPalette, PALETTE } from './pickers'
import { type Option, Select } from './Select'
import { Avatar, Confirm, Empty, errMsg, Field, Loading, type MenuItem, Modal, Segmented, Spinner, useAsync, useToast } from './ui'

const INTERVALS: Option<number>[] = [5, 10, 15, 30, 60, 120].map(s => ({ value: s, label: s < 60 ? `Every ${s} seconds` : s === 60 ? 'Every minute' : `Every ${s / 60} minutes` }))

export const KIND_LABEL: Record<ServerKind, string> = { local: 'This server', agent: 'Pocket Monitoring agent', glances: 'Glances' }

/** Opens the add-server dialog from anywhere (sidebar, empty states, palette). */
export const openAddServer = () => window.dispatchEvent(new CustomEvent('pmon:add-server'))

export function ServerDialog({ server, onClose, onSaved }: { server?: Server; onClose: () => void; onSaved?: (s: Server) => void }) {
  const { servers, reloadServers } = useApp()
  const { toast } = useToast()
  const isNew = !server
  const [f, setF] = useState({
    name: server?.name || '', kind: (server?.kind || 'agent') as ServerKind, url: server?.url || '', token: '', username: server?.username || '', password: '',
    color: server?.color || PALETTE[servers.length % PALETTE.length], interval_s: server?.interval_s || 10,
  })
  const [busy, setBusy] = useState(false)
  const [test, setTest] = useState<{ ok: boolean; hostname?: string; platform?: string; error?: string } | null>(null)
  const [testing, setTesting] = useState(false)
  const set = <K extends keyof typeof f>(k: K, v: (typeof f)[K]) => { setF(x => ({ ...x, [k]: v })); if (k !== 'name' && k !== 'color' && k !== 'interval_s') setTest(null) }
  const remote = f.kind !== 'local'
  const secretSaved = !!server?.has_secret

  const body = () => ({
    name: f.name.trim(), kind: f.kind, url: f.url.trim(), color: f.color, interval_s: f.interval_s,
    ...(f.kind === 'agent' ? (f.token ? { token: f.token } : {}) : {}),
    ...(f.kind === 'glances' ? { username: f.username.trim(), ...(f.password ? { password: f.password } : {}) } : {}),
  })
  const runTest = async () => {
    setTesting(true); setTest(null)
    try { setTest(await api.post(`${V1}/servers/test`, { ...body(), ...(server ? { id: server.id } : {}) })) } catch (e) { setTest({ ok: false, error: errMsg(e) }) } finally { setTesting(false) }
  }
  const save = async () => {
    setBusy(true)
    try {
      const b = { ...body(), name: f.name.trim() || test?.hostname || f.url.replace(/^https?:\/\//, '').replace(/[:/].*$/, '') }
      const s = isNew ? await api.post<Server>(`${V1}/servers`, b) : await api.put<Server>(`${V1}/servers/${server!.id}`, b)
      await reloadServers().catch(() => {})
      toast(isNew ? `“${s.name}” added — first readings arrive in a few seconds` : 'Saved')
      onSaved?.(s)
      onClose()
    } catch (e) { toast(errMsg(e), 'err') } finally { setBusy(false) }
  }
  const canSave = !busy && (!remote || !!f.url.trim()) && (!isNew || f.kind !== 'agent' || !!f.token)

  return (
    <Modal title={isNew ? 'Add a server' : `Edit ${server!.name}`} onClose={onClose} size="lg" footer={<>
      {remote && <button className="btn" disabled={testing || !f.url.trim()} onClick={runTest}>{testing ? <Spinner /> : <Icon name="activity" size={14} />}Test connection</button>}
      <span className="spacer" />
      <button className="btn" onClick={onClose}>Cancel</button>
      <button className="btn primary" disabled={!canSave} onClick={save}>{isNew ? 'Add server' : 'Save'}</button>
    </>}>
      <div className="stack">
        <div className="srv-name-row">
          <span className="srv-name-dot" style={{ background: f.color }} />
          <input className="srv-name-input" value={f.name} onChange={e => set('name', e.target.value)} placeholder={isNew ? 'Name, e.g. NAS (optional)' : 'Name'} autoFocus aria-label="Name" />
        </div>
        {f.kind === 'local' ? (
          <div className="callout info"><Icon name="info" size={15} /><span>This is the machine Pocket Monitoring runs on. It is read straight from the host — nothing to connect.</span></div>
        ) : (
          <>
            {isNew && (
              <Field label="How to reach it" group help={f.kind === 'agent'
                ? 'Run the small agent on that machine (see “Add another machine” in Settings → Servers). It has everything: processes, containers, sensors, RAID.'
                : 'Any machine already running Glances 4 in web-server mode (glances -w). No agent needed.'}>
                <Segmented label="Kind" value={f.kind} onChange={k => set('kind', k)} options={[
                  { value: 'agent', label: <span className="row"><Icon name="zap" size={14} />Pocket agent</span> },
                  { value: 'glances', label: <span className="row"><Icon name="activity" size={14} />Glances</span> },
                ]} />
              </Field>
            )}
            <Field label="Address" help={f.kind === 'agent' ? 'Where the agent listens, e.g. http://192.168.1.20:8193' : 'The Glances web server, e.g. http://192.168.1.20:61208'}>
              <input value={f.url} onChange={e => set('url', e.target.value)} placeholder={f.kind === 'agent' ? 'http://192.168.1.20:8193' : 'http://192.168.1.20:61208'} inputMode="url" autoComplete="off" spellCheck={false} />
            </Field>
            {f.kind === 'agent' && (
              <Field label="Agent token" help="The AGENT_TOKEN the agent was started with.">
                <input type="password" value={f.token} onChange={e => set('token', e.target.value)} placeholder={secretSaved ? '•••• saved — type to replace' : 'Paste the token'} autoComplete="new-password" spellCheck={false} />
              </Field>
            )}
            {f.kind === 'glances' && (
              <div className="two-col">
                <Field label="User name" help="Only when Glances runs with --password."><input value={f.username} onChange={e => set('username', e.target.value)} autoComplete="off" placeholder="glances" /></Field>
                <Field label="Password"><input type="password" value={f.password} onChange={e => set('password', e.target.value)} placeholder={secretSaved ? '•••• saved' : 'Optional'} autoComplete="new-password" /></Field>
              </div>
            )}
            {test && (test.ok
              ? <div className="callout success"><Icon name="check-circle" size={15} /><span>Connected to <b>{test.hostname || 'the server'}</b>{test.platform ? ` · ${test.platform}` : ''}.</span></div>
              : <div className="callout danger"><Icon name="alert" size={15} /><span>{test.error || 'Could not connect.'}</span></div>)}
          </>
        )}
        <div className="two-col">
          <Field label="Read it" group><Select value={f.interval_s} options={INTERVALS.some(o => o.value === f.interval_s) ? INTERVALS : [...INTERVALS, { value: f.interval_s, label: `Every ${f.interval_s} seconds` }]} onChange={v => set('interval_s', v)} label="Interval" /></Field>
          <Field label="Colour" group><ColorPalette value={f.color} onChange={c => set('color', c)} custom={false} /></Field>
        </div>
      </div>
    </Modal>
  )
}

export function ShareDialog({ server, onClose }: { server: Server; onClose: () => void }) {
  const { user, reloadServers } = useApp()
  const { toast } = useToast()
  const shares = useAsync(() => api.get<Share[]>(`${V1}/servers/${server.id}/shares`), [server.id])
  const people = useAsync(() => api.get<Person[]>(`${V1}/people`), [])
  const [pick, setPick] = useState<number | 'everyone' | 0>(0)
  const list = shares.data || []
  const everyone = list.some(s => s.everyone)
  const opts: Option<number | 'everyone' | 0>[] = [
    { value: 0, label: 'Choose a person…', disabled: true },
    ...(everyone ? [] : [{ value: 'everyone' as const, label: 'Everyone on this box', icon: <Icon name="users" size={14} /> }]),
    ...(people.data || []).filter(p => p.id !== user?.id && !list.some(s => s.user_id === p.id)).map(p => ({ value: p.id, label: p.name || p.email, description: p.name ? p.email : undefined })),
  ]
  const add = async () => {
    if (!pick) return
    try {
      await api.post(`${V1}/servers/${server.id}/shares`, pick === 'everyone' ? { everyone: true } : { user_id: pick })
      setPick(0); shares.reload(); reloadServers().catch(() => {})
    } catch (e) { toast(errMsg(e), 'err') }
  }
  const remove = async (s: Share) => {
    try { await api.del(`${V1}/shares/${s.id}`); shares.reload(); reloadServers().catch(() => {}) } catch (e) { toast(errMsg(e), 'err') }
  }
  return (
    <Modal title={`Share “${server.name}”`} onClose={onClose} footer={<button className="btn primary" onClick={onClose}>Done</button>}>
      <div className="stack">
        <p className="muted small" style={{ margin: 0 }}>People you share with can watch this server and its alerts. They can’t change it, its rules or its connection.</p>
        <div className="share-add">
          <div className="grow"><Select value={pick} options={opts} onChange={setPick} label="Person" searchable /></div>
          <button className="btn primary" disabled={!pick} onClick={add}>Share</button>
        </div>
        {!shares.data ? <Loading /> : !list.length ? <Empty title="Only you can see it" icon="lock" /> : (
          <div className="share-list">
            {list.map(s => (
              <div key={s.id} className="share-row">
                {s.everyone ? <span className="who-ico lg"><Icon name="users" size={15} /></span> : <Avatar name={s.person?.name} email={s.person?.email} size={30} />}
                <div className="grow">
                  <div className="share-name">{s.everyone ? 'Everyone on this box' : s.person?.name || s.person?.email}</div>
                  {!s.everyone && s.person?.name && <div className="muted small">{s.person.email}</div>}
                </div>
                <button className="btn ghost sm" onClick={() => remove(s)}>Remove</button>
              </div>
            ))}
          </div>
        )}
      </div>
    </Modal>
  )
}

/** Menu items + dialogs for a server; render `dialogs` once in the host. */
export function useServerActions() {
  const { reloadServers, bump } = useApp()
  const { toast } = useToast()
  const navigate = useNavigate()
  const [dialog, setDialog] = useState<{ kind: 'edit' | 'share' | 'delete' | 'leave'; server: Server } | null>(null)

  const setEnabled = useCallback(async (s: Server, enabled: boolean) => {
    try { await api.put(`${V1}/servers/${s.id}`, { enabled }); await reloadServers(); toast(enabled ? `Watching “${s.name}” again` : `“${s.name}” paused`) } catch (e) { toast(errMsg(e), 'err') }
  }, [reloadServers, toast])

  const items = (s: Server): MenuItem[] => s.role === 'owner' ? [
    { label: 'Open', icon: 'activity', onSelect: () => navigate(`/server/${s.id}`) },
    { label: 'Edit…', icon: 'edit', onSelect: () => setDialog({ kind: 'edit', server: s }) },
    { label: 'Share…', icon: 'share', onSelect: () => setDialog({ kind: 'share', server: s }) },
    { label: 'Alert rules', icon: 'bell', onSelect: () => navigate(`/settings/rules?server=${s.id}`) },
    s.enabled ? { label: 'Pause', icon: 'pause', onSelect: () => setEnabled(s, false) } : { label: 'Resume', icon: 'play', onSelect: () => setEnabled(s, true) },
    ...(s.kind !== 'local' ? ['sep' as const, { label: 'Delete…', icon: 'trash', danger: true, onSelect: () => setDialog({ kind: 'delete', server: s }) }] : []),
  ] : [
    { label: 'Open', icon: 'activity', onSelect: () => navigate(`/server/${s.id}`) },
    'sep',
    { label: 'Remove from my list', icon: 'log-out', danger: true, onSelect: () => setDialog({ kind: 'leave', server: s }) },
  ]

  const confirm = async () => {
    const d = dialog
    setDialog(null)
    if (!d) return
    try {
      if (d.kind === 'delete') await api.del(`${V1}/servers/${d.server.id}`)
      else await api.post(`${V1}/servers/${d.server.id}/leave`)
      await reloadServers(); bump()
      toast(d.kind === 'delete' ? `“${d.server.name}” deleted` : `Removed “${d.server.name}”`)
      if (location.pathname === `/server/${d.server.id}`) navigate('/')
    } catch (e) { toast(errMsg(e), 'err') }
  }

  const dialogs: React.ReactNode = <>
    {dialog?.kind === 'edit' && <ServerDialog server={dialog.server} onClose={() => setDialog(null)} />}
    {dialog?.kind === 'share' && <ShareDialog server={dialog.server} onClose={() => setDialog(null)} />}
    {(dialog?.kind === 'delete' || dialog?.kind === 'leave') && (
      <Confirm title={dialog.kind === 'delete' ? `Delete “${dialog.server.name}”?` : `Remove “${dialog.server.name}”?`}
        message={dialog.kind === 'delete' ? 'Its history, alert rules and alerts are deleted, for everyone it’s shared with. The machine itself is not touched.' : <>It stops showing up for you. {dialog.server.owner?.name || 'The owner'} can share it again.</>}
        confirmLabel={dialog.kind === 'delete' ? 'Delete server' : 'Remove'} onCancel={() => setDialog(null)} onConfirm={confirm} />
    )}
  </>
  return { items, dialogs, edit: (s: Server) => setDialog({ kind: 'edit', server: s }), share: (s: Server) => setDialog({ kind: 'share', server: s }) }
}

/** Listens for openAddServer() and shows the dialog. */
export function AddServerHost() {
  const navigate = useNavigate()
  const [open, setOpen] = useState(false)
  useEffect(() => {
    const h = () => setOpen(true)
    window.addEventListener('pmon:add-server', h)
    return () => window.removeEventListener('pmon:add-server', h)
  }, [])
  return open ? <ServerDialog onClose={() => setOpen(false)} onSaved={s => navigate(`/server/${s.id}`)} /> : null
}
