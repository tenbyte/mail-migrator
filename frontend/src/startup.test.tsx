import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import { StartupNotice } from './App'

describe('startup notice', () => {
  it('renders without an update message when no newer release exists', () => {
    const html = renderToStaticMarkup(<StartupNotice update={{ currentVersion: '0.3.0', latestVersion: '0.3.0', updateAvailable: false }} onClose={() => undefined} onViewRelease={() => undefined}/>)
    expect(html).toContain('Use at your own risk')
    expect(html).toContain('You are responsible for the accounts, settings, and actions you choose in this app.')
    expect(html).toContain('Keep an independent backup')
    expect(html).toContain('IMAP mail migration is supported')
    expect(html).toContain('Courier artwork adapted from the Go Gopher by Renee French under CC BY 4.0.')
    expect(html).toContain('not endorsed by the Go project or Google')
    expect(html).not.toContain('View release')
  })

  it('renders the current and latest versions when an update exists', () => {
    const html = renderToStaticMarkup(<StartupNotice update={{ currentVersion: '0.3.0', latestVersion: '0.3.1', updateAvailable: true }} onClose={() => undefined} onViewRelease={() => undefined}/>)
    expect(html).toContain('Version 0.3.1 is available. You are running 0.3.0.')
    expect(html).toContain('View release')
  })
})
