const { execFileSync } = require('node:child_process')
const { mkdirSync } = require('node:fs')
const { join, resolve } = require('node:path')
const { Arch } = require('electron-builder')
const { version } = require('./package.json')

module.exports = {
  appId: 'network.sisyphus.desktop',
  productName: 'Sisyphus',
  executableName: 'sisyphus',
  directories: { output: 'dist' },
  files: ['out/**/*', 'package.json'],
  asar: true,
  extraResources: [{ from: '.packaging/bin', to: 'bin', filter: ['sisyphusd', 'sisyphusd.exe'] }],
  artifactName: '${productName}-${version}-${os}-${arch}.${ext}',
  linux: { target: ['tar.gz'], category: 'Science' },
  win: { target: ['zip'] },
  mac: { target: ['zip'], category: 'public.app-category.productivity' },
  // Build on each target OS; do not accidentally ship a host-architecture daemon.
  beforePack: async ({ electronPlatformName, arch }) => {
    if (electronPlatformName !== process.platform || Arch[arch] !== process.arch) {
      throw new Error('Build Sisyphus on the matching operating system and architecture.')
    }
    const directory = resolve(__dirname, '.packaging/bin')
    mkdirSync(directory, { recursive: true })
    const binary = join(directory, process.platform === 'win32' ? 'sisyphusd.exe' : 'sisyphusd')
    const platforms = { linux: 'linux', darwin: 'darwin', win32: 'windows' }
    const architectures = { x64: 'amd64', arm64: 'arm64' }
    if (!platforms[process.platform] || !architectures[process.arch]) throw new Error('Unsupported native packaging target.')
    execFileSync('go', ['build', '-trimpath', '-ldflags', `-s -w -X main.version=${version}`, '-o', binary, './apps/sisyphusd'], {
      cwd: resolve(__dirname, '../..'), stdio: 'inherit',
      env: { ...process.env, GOOS: platforms[process.platform], GOARCH: architectures[process.arch] },
    })
  },
}
