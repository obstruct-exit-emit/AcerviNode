import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import App from './App.tsx'
import { DownloadWindow } from './components/DownloadWindow.tsx'

// The small popup window opened by openDownloadWindow() (see fsAccess.ts)
// loads this exact same bundle/entry point, just with ?popup=downloads in
// the URL — simplest way to get a second, minimal UI without a separate
// Vite build target or an extra route in the Go backend.
const isDownloadPopup = new URLSearchParams(window.location.search).get('popup') === 'downloads'

createRoot(document.getElementById('root')!).render(
  <StrictMode>{isDownloadPopup ? <DownloadWindow /> : <App />}</StrictMode>,
)
