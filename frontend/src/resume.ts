import type { ResumeJobRequest, ResumeRequirements, ServiceKind } from './types'

export type ResumePasswords = Partial<Record<ServiceKind, { source: string; destination: string }>>

export function resumeCredentialsValid(requirements: ResumeRequirements, passwords: ResumePasswords): boolean {
  return requirements.credentials.every(item =>
    Boolean(item.sourceAvailable || passwords[item.kind]?.source) &&
    Boolean(item.destinationAvailable || passwords[item.kind]?.destination),
  )
}

export function buildResumeJobRequest(requirements: ResumeRequirements, passwords: ResumePasswords, rememberNewCredentials: boolean): ResumeJobRequest {
  const credentials: ResumeJobRequest['credentials'] = {}
  for (const requirement of requirements.credentials) {
    credentials[requirement.kind] = {
      sourcePassword: passwords[requirement.kind]?.source ?? '',
      destinationPassword: passwords[requirement.kind]?.destination ?? '',
    }
  }
  return { migrationId: requirements.migration.id, credentials, rememberNewCredentials }
}
