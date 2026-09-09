import { Component, type ErrorInfo, type ReactNode } from 'react'
import { backend } from './backend'
import { reportFrontendError } from './diagnostics'

type FrontendErrorState = { error: string; stack: string; componentStack: string; logDirectory: string; copied: boolean; copyError: string }

export function fallbackDiagnosticsText(state: Pick<FrontendErrorState, 'error' | 'stack' | 'componentStack'>) {
  return ['Tenbyte Mail Migrator frontend crash', '', 'message:', state.error, '', 'stack:', state.stack, '', 'react component stack:', state.componentStack].join('\n')
}

export class FrontendErrorBoundary extends Component<{ children: ReactNode }, FrontendErrorState> {
  state: FrontendErrorState = { error: '', stack: '', componentStack: '', logDirectory: '', copied: false, copyError: '' }

  static getDerivedStateFromError(error: unknown) {
    const value = error instanceof Error ? error : new Error(String(error))
    return { error: value.message, stack: value.stack ?? '', copied: false, copyError: '' }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('Frontend could not be rendered', error, info)
    this.setState({ componentStack: info.componentStack ?? '' })
    void reportFrontendError(error, info.componentStack ?? '', true)
    void backend.diagnosticsInfo().then(value => this.setState({ logDirectory: value.logDirectory })).catch(() => undefined)
  }

  async copyDiagnostics() {
    this.setState({ copyError: '', copied: false })
    try {
      await backend.copyDiagnostics()
      this.setState({ copied: true })
    } catch (cause) {
      try {
        await navigator.clipboard.writeText(fallbackDiagnosticsText(this.state))
        this.setState({ copied: true })
      } catch {
        this.setState({ copyError: cause instanceof Error ? cause.message : String(cause) })
      }
    }
  }

  render() {
    if (this.state.error) {
      return <main className="fatal-error" role="alert">
        <span className="eyebrow">Frontend error</span>
        <h1>The interface could not be loaded.</h1>
        <p>{this.state.error}</p>
        {this.state.logDirectory && <p className="diagnostics-path">Diagnostic files: <code>{this.state.logDirectory}</code></p>}
        {this.state.copyError && <p className="diagnostics-copy-error">{this.state.copyError}</p>}
        <div className="fatal-actions">
          <button className="secondary" onClick={() => void this.copyDiagnostics()}>{this.state.copied ? 'Diagnostics copied' : 'Copy diagnostics'}</button>
          <button className="primary" onClick={() => window.location.reload()}>Reload</button>
        </div>
      </main>
    }
    return this.props.children
  }
}
