import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { backend } from './backend'

beforeEach(() => {
  Object.defineProperty(globalThis, 'window', { value: {}, configurable: true, writable: true })
})

afterEach(() => {
  Reflect.deleteProperty(globalThis, 'window')
})

describe('reset backend bindings', () => {
  it('calls the migration-data reset binding', async () => {
    const reset = vi.fn().mockResolvedValue(undefined)
    window.go = { main: { App: { ResetMigrationData: reset } as never } }
    await backend.resetMigrationData()
    expect(reset).toHaveBeenCalledOnce()
  })

  it('calls the factory-reset binding', async () => {
    const reset = vi.fn().mockResolvedValue(undefined)
    window.go = { main: { App: { FactoryReset: reset } as never } }
    await backend.factoryReset()
    expect(reset).toHaveBeenCalledOnce()
  })
})
