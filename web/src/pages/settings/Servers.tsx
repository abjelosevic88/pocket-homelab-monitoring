import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { api, V1 } from '../../lib/api'
import { useApp } from '../../lib/app-context'
import { ago } from '../../lib/format'
import { Icon } from '../../components/icons'
import { KIND_LABEL, openAddServer, useServerActions } from '../../components/ServerDialogs'
import { serverSubtitle, ServerState } from '../../components/Status'
import { CopyField, Empty, errMsg, Loading, MenuButton, Segmented, Switch, useToast } from '../../components/ui'
import { Section } from './common'

function randomToken() {
  const b = new Uint8Array(24)
  crypto.getRandomValues(b)
  return Array.from(b, x => x.toString(16).padStart(2, '0')).join('')
}

export function ServersTab() {
  const { servers, serversLoaded, reloadServers } = useApp()
  const { toast } = useToast()
  const actions = useServerActions()
  const mine = servers.filter(s => s.role === 'owner')
  const shared = servers.filter(s => s.role !== 'owner')

  const setEnabled = async (id: number, enabled: boolean) => {
    try { await api.put(`${V1}/servers/${id}`, { enabled }); await reloadServers() } catch (e) { toast(errMsg(e), 'err') }
  }

  return (
    <div className="stack loose">
      <Section title="Your servers" sub="Only you see these, unless you share one." actions={<button className="btn primary sm" onClick={openAddServer}><Icon name="plus" size={14} />Add server</button>}>
        {!serversLoaded ? <Loading /> : !mine.length ? <Empty title="No servers yet" icon="server">Add one with the button above.</Empty> : (
          <div className="set-cal-list">
            {mine.map(s => (
              <div key={s.id} className="set-cal-row">
                <span className="set-cal-dot" style={{ background: s.color }} />
                <div className="grow">
                  <div className="set-cal-name"><Link to={`/server/${s.id}`}>{s.name}</Link><ServerState status={s.status} />{(s.shares?.length ?? 0) > 0 && <Icon name="users" size={13} className="faint" title="Shared" />}</div>
                  <div className="set-cal-meta">{KIND_LABEL[s.kind]} · {serverSubtitle(s)} · every {s.interval_s}s{s.last_seen ? ` · last reading ${ago(s.last_seen)}` : ''}</div>
                  {s.last_error && s.status !== 'up' && <div className="acct-error">{s.last_error}</div>}
                </div>
                <Switch label={s.enabled ? `Pause ${s.name}` : `Resume ${s.name}`} checked={s.enabled} onChange={v => setEnabled(s.id, v)} />
                <button className="btn sm" onClick={() => actions.edit(s)}>Edit</button>
                <MenuButton label={`More for ${s.name}`} align="right" items={actions.items(s)} width={210}><Icon name="more" size={16} /></MenuButton>
              </div>
            ))}
          </div>
        )}
      </Section>

      {shared.length > 0 && (
        <Section title="Shared with you" sub="You can watch these; their owners manage them.">
          <div className="set-cal-list">
            {shared.map(s => (
              <div key={s.id} className="set-cal-row">
                <span className="set-cal-dot" style={{ background: s.color }} />
                <div className="grow">
                  <div className="set-cal-name"><Link to={`/server/${s.id}`}>{s.name}</Link><ServerState status={s.status} /></div>
                  <div className="set-cal-meta">Shared by {s.owner?.name || s.owner?.email} · {serverSubtitle(s)}</div>
                </div>
                <MenuButton label={`More for ${s.name}`} align="right" items={actions.items(s)} width={210}><Icon name="more" size={16} /></MenuButton>
              </div>
            ))}
          </div>
        </Section>
      )}

      <AddMachineHelp />
      {actions.dialogs}
    </div>
  )
}

function AddMachineHelp() {
  const [how, setHow] = useState<'docker' | 'compose' | 'glances'>('docker')
  const token = useMemo(randomToken, [])
  const image = 'ghcr.io/abjelosevic88/pocket-homelab-monitoring:latest'
  const docker = `docker run -d --name pocket-monitoring-agent --restart unless-stopped --user 0:0 --pid host --network host -v /:/host/root:ro,rslave -v /var/run/docker.sock:/var/run/docker.sock:ro -e AGENT_TOKEN=${token} -e AGENT_PORT=8193 ${image} agent`
  const compose = `services:
  pocket-monitoring-agent:
    image: ${image}
    command: agent
    container_name: pocket-monitoring-agent
    restart: unless-stopped
    user: "0:0"
    pid: host
    network_mode: host
    environment:
      AGENT_TOKEN: ${token}
      AGENT_PORT: "8193"
    volumes:
      - /:/host/root:ro,rslave
      - /var/run/docker.sock:/var/run/docker.sock:ro`
  return (
    <Section title="Add another machine" sub="Run the agent on the machine you want to watch, then add it here with its address and the token.">
      <div className="stack">
        <Segmented label="How" value={how} onChange={setHow} options={[{ value: 'docker', label: 'docker run' }, { value: 'compose', label: 'Compose' }, { value: 'glances', label: 'Already have Glances' }]} />
        {how !== 'glances' ? <>
          <ol className="steps">
            <li>On that machine, start the agent (the token below is freshly made for it — keep it):
              {how === 'docker' ? <CopyField value={docker} label="docker run command" /> : <><pre className="mono small">{compose}</pre><CopyField value={compose} label="Compose service" /></>}
            </li>
            <li>Make sure this box can reach port <code>8193</code> on it (same LAN or tailnet).</li>
            <li>Click <b>Add server</b>, choose <b>Pocket agent</b>, enter <code>http://&lt;its address&gt;:8193</code> and this token: <CopyField value={token} label="Agent token" secret /></li>
          </ol>
        </> : (
          <ol className="steps">
            <li>Run Glances 4 in web-server mode on that machine (<code>glances -w</code>, or the <code>nicolargo/glances</code> image with <code>GLANCES_OPT=-w</code>).</li>
            <li>Click <b>Add server</b>, choose <b>Glances</b> and enter <code>http://&lt;its address&gt;:61208</code> (plus the user and password if you set <code>--password</code>).</li>
            <li>Glances gives everything except the per-process container names and RAID details.</li>
          </ol>
        )}
        <div className="row"><button className="btn primary" onClick={openAddServer}><Icon name="plus" size={14} />Add server</button></div>
      </div>
    </Section>
  )
}
