export function formatNumber(value = 0) { return new Intl.NumberFormat().format(value) }

export function formatBytes(value = 0) {
  if (!Number.isFinite(value) || value <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const index = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1)
  return `${new Intl.NumberFormat(undefined, { maximumFractionDigits: index > 1 ? 1 : 0 }).format(value / 1024 ** index)} ${units[index]}`
}

export function humanState(state: string) {
  return ({ CREATED: 'Prepared', READY: 'Ready', RUNNING: 'Running', PAUSED: 'Paused', INTERRUPTED: 'Interrupted', COMPLETED: 'Completed', COMPLETED_WITH_ERRORS: 'Completed with issues', FAILED: 'Failed', CANCELLED: 'Cancelled' } as Record<string,string>)[state] ?? state
}
