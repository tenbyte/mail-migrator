import { backend } from './backend'
import type { FrontendErrorReport } from './types'

let currentView = 'startup'
const reported = new Set<string>()

export function setDiagnosticView(view: string) {
  currentView = view
}

export function toFrontendErrorReport(cause: unknown, componentStack = '', fatal = false): FrontendErrorReport {
  const error = cause instanceof Error ? cause : new Error(String(cause))
  return {
    message: error.message || error.name || 'Unknown frontend error',
    stack: error.stack ?? '',
    componentStack,
    view: currentView,
    fatal,
  }
}

export function frontendErrorFingerprint(report: FrontendErrorReport) {
  return [report.fatal ? 'fatal' : 'error', report.view ?? '', report.message, report.stack ?? '', report.componentStack ?? ''].join('\u0000')
}

export function reportFrontendError(cause: unknown, componentStack = '', fatal = false) {
  const report = toFrontendErrorReport(cause, componentStack, fatal)
  const fingerprint = frontendErrorFingerprint(report)
  if (reported.has(fingerprint)) return Promise.resolve()
  reported.add(fingerprint)
  return backend.reportFrontendError(report).catch(() => undefined)
}

export function installGlobalErrorHandlers() {
  window.addEventListener('error', event => {
    void reportFrontendError(event.error ?? event.message, '', false)
  })
  window.addEventListener('unhandledrejection', event => {
    void reportFrontendError(event.reason, '', false)
  })
}

export function resetReportedErrorsForTest() {
  reported.clear()
  currentView = 'startup'
}
