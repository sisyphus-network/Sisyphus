export function getBrowserDeviceName(): string {
  const userAgent = navigator.userAgent
  const browser = /Edg\//.test(userAgent) ? 'Edge'
    : /Firefox\//.test(userAgent) ? 'Firefox'
      : /Chrome\//.test(userAgent) ? 'Chrome'
        : /Safari\//.test(userAgent) ? 'Safari' : 'Browser'
  const platform = /Android/i.test(userAgent) ? 'Android'
    : /iPhone|iPad|iPod/i.test(userAgent) ? 'iOS'
      : /Windows/i.test(userAgent) ? 'Windows'
        : /Macintosh|Mac OS/i.test(userAgent) ? 'Mac'
          : /Linux/i.test(userAgent) ? 'Linux' : ''
  return platform ? `${browser} on ${platform}` : browser
}
