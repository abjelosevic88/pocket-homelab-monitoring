import React from 'react'
import ReactDOM from 'react-dom/client'
import { BrowserRouter, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import './styles/base.css'
import './styles/app.css'
import './styles/monitor.css'
import './styles/settings.css'
import { AppProvider, useApp } from './lib/app-context'
import { Loading, ToastProvider } from './components/ui'
import { ErrorBoundary } from './components/ErrorBoundary'
import Login from './pages/Login'
import Overview from './pages/Overview'
import ServerPage from './pages/ServerPage'
import AlertsPage from './pages/Alerts'
import Settings from './pages/Settings'

function Gate({ children }: { children: React.ReactNode }) {
  const { status, user, settings } = useApp()
  const loc = useLocation()
  if (!status) return <div className="boot"><img src="/favicon.svg" alt="" /><Loading /></div>
  if (status.needs_setup) return <Navigate to="/setup" replace />
  if (!user) return <Navigate to="/login" replace state={{ from: loc.pathname + loc.search }} />
  if (!settings) return <div className="boot"><img src="/favicon.svg" alt="" /><Loading /></div>
  return <>{children}</>
}

function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login mode="login" />} />
      <Route path="/setup" element={<Login mode="setup" />} />
      <Route path="/settings/:tab?" element={<Gate><ErrorBoundary where="settings"><Settings /></ErrorBoundary></Gate>} />
      <Route path="/server/:id" element={<Gate><ErrorBoundary where="server"><ServerPage /></ErrorBoundary></Gate>} />
      <Route path="/alerts" element={<Gate><ErrorBoundary where="alerts"><AlertsPage /></ErrorBoundary></Gate>} />
      <Route path="/" element={<Gate><ErrorBoundary where="overview"><Overview /></ErrorBoundary></Gate>} />
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <BrowserRouter>
      <ErrorBoundary where="app"><ToastProvider><AppProvider><App /></AppProvider></ToastProvider></ErrorBoundary>
    </BrowserRouter>
  </React.StrictMode>,
)
