// Runtime facts fetched from GET /api/status before the console renders.
let testMode = false
let database = 'sqlite'

export function setRuntimeStatus(status: { test_mode?: boolean; database?: string }): void {
  testMode = Boolean(status.test_mode)
  if (status.database) database = status.database
}

export function isTestMode(): boolean {
  return testMode
}

export function databaseBackend(): string {
  return database
}
