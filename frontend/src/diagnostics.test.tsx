import { renderToStaticMarkup } from 'react-dom/server'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fallbackDiagnosticsText, FrontendErrorBoundary } from './FrontendErrorBoundary'
import { frontendErrorFingerprint, resetReportedErrorsForTest, toFrontendErrorReport } from './diagnostics'

afterEach(() => {
  resetReportedErrorsForTest()
  vi.restoreAllMocks()
})

function nullMapError() {
  try {
    const value = null as unknown as string[]
    value.map(item => item)
  } catch (cause) {
    return cause
  }
  throw new Error('expected null.map to fail')
}

describe('frontend diagnostics', () => {
  it('keeps the fatal fallback usable for a null.map render failure', () => {
    const cause = nullMapError()
    const state = FrontendErrorBoundary.getDerivedStateFromError(cause)
    const boundary = new FrontendErrorBoundary({ children: <div/> })
    boundary.state = { ...boundary.state, ...state, logDirectory: '/safe/logs' }
    const html = renderToStaticMarkup(boundary.render())

    expect(html).toContain('The interface could not be loaded.')
    expect(html).toContain('reading')
    expect(html).toContain('Copy diagnostics')
    expect(html).toContain('/safe/logs')
    expect(html).toContain('Reload')
  })

  it('builds a stable report with stack, component stack, and fatal state', () => {
    const cause = nullMapError()
    const first = toFrontendErrorReport(cause, '\n at ResumeDialog', true)
    const second = toFrontendErrorReport(cause, '\n at ResumeDialog', true)

    expect(first.message).toContain('reading')
    expect(first.stack).toContain('TypeError')
    expect(first.componentStack).toContain('ResumeDialog')
    expect(first.fatal).toBe(true)
    expect(frontendErrorFingerprint(first)).toBe(frontendErrorFingerprint(second))
  })

  it('keeps a self-contained clipboard fallback when backend persistence is unavailable', () => {
    const text = fallbackDiagnosticsText({ error: 'null.map failed', stack: 'TypeError at App.tsx:1', componentStack: 'at ResumeDialog' })
    expect(text).toContain('null.map failed')
    expect(text).toContain('TypeError at App.tsx:1')
    expect(text).toContain('at ResumeDialog')
  })
})
