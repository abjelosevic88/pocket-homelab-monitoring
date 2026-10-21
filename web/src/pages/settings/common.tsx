import React, { useCallback } from 'react'
import { useApp } from '../../lib/app-context'
import type { Settings } from '../../lib/types'
import { errMsg, useToast } from '../../components/ui'

/** saveSettings with an error toast (the context already rolls back). */
export function useSaveSettings() {
  const { saveSettings } = useApp()
  const { toast } = useToast()
  return useCallback(async (patch: Partial<Settings> & { notify_token?: string }, ok?: string) => {
    try { await saveSettings(patch); if (ok) toast(ok); return true } catch (e) { toast(errMsg(e), 'err'); return false }
  }, [saveSettings, toast])
}

export function StatusDot({ status, title }: { status: 'ok' | 'error' | 'off' | 'warn'; title?: string }) {
  return <span className={`set-status ${status}`} title={title} aria-label={title || status} role="img" />
}

export function Section({ title, sub, actions, children }: { title: React.ReactNode; sub?: React.ReactNode; actions?: React.ReactNode; children: React.ReactNode }) {
  return (
    <section className="set-section">
      <div className="set-section-head">
        <div className="grow"><h3>{title}</h3>{sub && <p>{sub}</p>}</div>
        {actions && <div className="row">{actions}</div>}
      </div>
      <div className="set-section-body">{children}</div>
    </section>
  )
}
