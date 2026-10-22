import { useEffect, useState } from 'react'

/** Ask in the header on phones; on a desktop the floating Pocket Ask launcher
 *  sits in the bottom-right corner instead. While this button is on the page
 *  the launcher hides its own floating pill ([data-pocket-ask-trigger]). */
export function AskButton() {
  const [available, setAvailable] = useState(() => !!document.querySelector('pocket-ask-launcher'))
  useEffect(() => {
    const ready = () => setAvailable(!!document.querySelector('pocket-ask-launcher'))
    window.addEventListener('pocket-ask-available', ready)
    ready()
    return () => window.removeEventListener('pocket-ask-available', ready)
  }, [])
  if (!available) return null
  return (
    <button type="button" className="btn ghost ask-header-btn" data-pocket-ask-trigger
      aria-label="Ask AI about this page" title="Ask AI about this page" aria-haspopup="dialog"
      style={{ gap: 5, padding: '0 10px', flexShrink: 0 }}
      onClick={() => window.dispatchEvent(new CustomEvent('pocket-ask'))}>
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
        <path d="M9.9 4.6 11 8l3.4 1.1L11 10.2 9.9 13.6 8.8 10.2 5.4 9.1 8.8 8Z" /><path d="M17 3v3M15.5 4.5h3M17.5 14.5v4M15.5 16.5h4" />
      </svg>
      <span>Ask</span>
    </button>
  )
}
