import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import App from './App.tsx'
import ErrorBoundary from './components/ErrorBoundary'
import { initI18n } from './i18n'

// Wait for the locale bundle before the first render: painting first and switching later would
// show a frame of English keys to a Chinese or Arabic user.
void initI18n().then(() => {
  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      {/* Outside the router on purpose: a crash in any page must not take the whole document down
          to a white screen, and it catches route-level crashes too. */}
      <ErrorBoundary>
        <App />
      </ErrorBoundary>
    </StrictMode>,
  )
})
