import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import App from './App.tsx'
import { registerServiceWorker } from './lib/registerSW.ts'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)

// After the render call rather than before it: the worker supplies a page for a
// navigation that fails, which is of no use to a tab that has already loaded,
// so it must not compete with the first paint. Nothing awaits it -- the app
// neither knows nor cares whether there is a worker.
void registerServiceWorker()
