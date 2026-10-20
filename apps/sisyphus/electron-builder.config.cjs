const { execFileSync } = require('node:child_process')
const { mkdirSync } = require('node:fs')
const { join, resolve } = require('node:path')
const { Arch } = require('electron-builder')
const { version } = require('./package.json')

module.exports = {
  appId: 'network.sisyphus.desktop',
  productName: 'Sisyphus',
  // Windows shows the author from package.json as the publisher in Apps and
  // Features and in the file's details; the makers are named by GitHub handle.
  copyright: 'Copyright © 2026 rvnminers-A-and-N and excho0. Apache License 2.0.',
  executableName: 'sisyphus',
  directories: { output: 'dist' },
  files: ['out/**/*', 'package.json'],
  asar: true,
  extraResources: [
    { from: '.packaging/bin', to: 'bin', filter: ['sisyphusd', 'sisyphusd.exe'] },
    // Linux takes a window's icon from the window, not from the package.
    { from: 'build/icons/256x256.png', to: 'icon.png' },
    // The tray icon in each node state, drawn by scripts/make-icons.py.
    { from: 'build/tray', to: 'tray' },
  ],
  artifactName: '${productName}-${version}-${os}-${arch}.${ext}',
  // The icons in build/ are drawn from assets/logo.png by scripts/make-icons.py.
  linux: { target: ['tar.gz', 'AppImage'], category: 'Science', icon: 'build/icons', synopsis: 'A node of the Sisyphus network' },
  win: { target: ['zip', 'nsis'], icon: 'build/icon.ico' },
  // An installer a person steps through, so that Windows shows where it goes
  // and puts the logo in the Start menu and on the desktop.
  nsis: {
    oneClick: false, perMachine: false, allowToChangeInstallationDirectory: true,
    installerIcon: 'build/icon.ico', uninstallerIcon: 'build/icon.ico',
    createDesktopShortcut: true, createStartMenuShortcut: true, shortcutName: 'Sisyphus',
    artifactName: '${productName}-${version}-${os}-${arch}-setup.${ext}',
  },
  mac: { target: ['zip', 'dmg'], category: 'public.app-category.productivity', icon: 'build/icon.icns' },
  // The disk image a person opens to drag the app onto Applications.
  dmg: {
    icon: 'build/icon.icns', title: '${productName} ${version}', window: { width: 540, height: 380 },
    contents: [{ x: 140, y: 190 }, { x: 400, y: 190, type: 'link', path: '/Applications' }],
  },
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
