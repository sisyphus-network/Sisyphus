export function packageResourcesDirectory(platform, architecture) {
  const suffix = architecture === 'arm64' ? '-arm64' : ''
  if (platform === 'linux') return `linux${suffix}-unpacked/resources`
  if (platform === 'win32') return `win${suffix}-unpacked/resources`
  if (platform === 'darwin') {
    return `${architecture === 'arm64' ? 'mac-arm64' : 'mac'}/Sisyphus.app/Contents/Resources`
  }
  throw new Error(`Unsupported package platform: ${platform}`)
}
