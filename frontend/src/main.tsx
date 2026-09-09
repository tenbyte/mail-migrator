import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'
import { FrontendErrorBoundary } from './FrontendErrorBoundary'
import { installGlobalErrorHandlers } from './diagnostics'
import './styles.css'

installGlobalErrorHandlers()

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode><FrontendErrorBoundary><App /></FrontendErrorBoundary></React.StrictMode>,
)
