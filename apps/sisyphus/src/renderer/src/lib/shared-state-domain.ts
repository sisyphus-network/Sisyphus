// grpc-js with proto-loader's `enums: String` option returns the protobuf
// enum name, while browser Connect clients return the numeric enum value.
export type SharedStateKind = 'node' | 'workers' | 'joinedPool' | 'modelProfiles' | 'modelLending' | 'files' | 'poolLimits' | 'locationDiscovery' | 'webAccess' | 'poolMembers'

export function sharedStateDomainCode(domain: number | string): SharedStateKind | undefined {
  switch (domain) {
    case 1:
    case 'SHARED_STATE_DOMAIN_NODE': return 'node'
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
    case 9:
    case 'SHARED_STATE_DOMAIN_POOL_LIMITS': return 'poolLimits'
    case 10:
    case 'SHARED_STATE_DOMAIN_LOCATION_DISCOVERY': return 'locationDiscovery'
    case 11:
    case 'SHARED_STATE_DOMAIN_WEB_ACCESS': return 'webAccess'
    case 12:
    case 'SHARED_STATE_DOMAIN_POOL_MEMBERS': return 'poolMembers'
    default: return undefined
  }
}
