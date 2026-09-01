import React, { Component, type ErrorInfo, type ReactNode } from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'
import './styles.css'

class FrontendErrorBoundary extends Component<{ children: ReactNode }, { error: string }> {
  state = { error: '' }

  static getDerivedStateFromError(error: unknown) {
    return { error: error instanceof Error ? error.message : String(error) }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('Frontend could not be rendered', error, info)
  }

  render() {
    if (this.state.error) {
      return <main className="fatal-error" role="alert">
        <span className="eyebrow">Frontend error</span>
        <h1>The interface could not be loaded.</h1>
        <p>{this.state.error}</p>
        <button className="primary" onClick={() => window.location.reload()}>Neu laden</button>
      </main>
    }
    return this.props.children
  }
}

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode><FrontendErrorBoundary><App /></FrontendErrorBoundary></React.StrictMode>,
)
