// grpc-js with proto-loader's `enums: String` option returns the protobuf
// enum name, while browser Connect clients return the numeric enum value.
export type SharedStateKind = 'workers' | 'joinedPool' | 'modelProfiles' | 'modelLending' | 'files'

export function sharedStateDomainCode(domain: number | string): SharedStateKind | undefined {
  switch (domain) {
    case 3:
    case 'SHARED_STATE_DOMAIN_WORKERS': return 'workers'
    case 5:
    case 'SHARED_STATE_DOMAIN_JOINED_POOL': return 'joinedPool'
    case 6:
    case 'SHARED_STATE_DOMAIN_MODEL_PROFILES': return 'modelProfiles'
    case 7:
    case 'SHARED_STATE_DOMAIN_MODEL_LENDING': return 'modelLending'
    case 8:
    case 'SHARED_STATE_DOMAIN_FILES': return 'files'
    default: return undefined
  }
}
