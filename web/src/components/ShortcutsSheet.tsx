import { Modal } from './ui'

const GROUPS: { title: string; keys: [string[], string][] }[] = [
  { title: 'Navigate', keys: [[['g', 'o'], 'Overview'], [['g', 'a'], 'Alerts'], [['g', 's'], 'Settings'], [['/'], 'Search & commands'], [['⌘', 'K'], 'Search & commands']] },
  { title: 'Server page', keys: [[['1'], 'Live'], [['2'], 'Last hour'], [['3'], 'Last 6 hours'], [['4'], 'Last 24 hours'], [['5'], 'Last 7 days'], [['6'], 'Last 30 days'], [['['], 'Previous section'], [[']'], 'Next section']] },
  { title: 'Servers', keys: [[['n'], 'Add a server'], [['j'], 'Next server'], [['k'], 'Previous server']] },
  { title: 'Charts', keys: [[['←', '→'], 'Step through points (chart focused)'], [['esc'], 'Hide the readout']] },
  { title: 'Help', keys: [[['?'], 'This sheet']] },
]

export function ShortcutsSheet({ onClose }: { onClose: () => void }) {
  return (
    <Modal title="Keyboard shortcuts" onClose={onClose} size="lg">
      <div className="shortcuts">
        {GROUPS.map(g => (
          <section key={g.title}>
            <h4 className="sc-h">{g.title}</h4>
            {g.keys.map(([keys, label]) => (
              <div key={label + keys.join('')} className="sc-row"><span>{label}</span><span className="sc-keys">{keys.map(k => <kbd key={k}>{k}</kbd>)}</span></div>
            ))}
          </section>
        ))}
      </div>
    </Modal>
  )
}
