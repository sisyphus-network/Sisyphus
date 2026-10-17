const hostnameLabel = /^(?!-)[a-z0-9-]{1,63}(?<!-)$/i

function validHostname(host: string): boolean {
  const name = host.endsWith('.') ? host.slice(0, -1) : host
  return name.length > 0 && name.length <= 253 && name.split('.').every((label) => hostnameLabel.test(label))
}

function validPort(port: string | undefined): boolean {
  if (port === undefined) return true
  if (!/^\d{1,5}$/.test(port)) return false
  const value = Number(port)
  return value > 0 && value <= 65535
}

function validBareIPv6(host: string): boolean {
  if (!host.includes(':') || !/^[0-9a-f:.]+$/i.test(host)) return false
  try {
    const parsed = new URL(`http://[${host}]/`)
    return parsed.hostname.startsWith('[') && parsed.hostname.endsWith(']')
  } catch {
    return false
  }
}

export function normalizeStunServer(value: unknown): string {
  if (typeof value !== 'string') throw new Error('Enter a STUN server as host[:port].')
  const server = value.trim()
  if (!server) return ''
  if (server.length > 260) throw new Error('Enter a STUN server as host[:port].')

  if (server.startsWith('[')) {
    const match = /^\[([0-9a-f:.]+)\](?::(\d{1,5}))?$/i.exec(server)
    if (!match || !match[1].includes(':') || !validPort(match[2])) throw new Error('Enter a STUN server as host[:port].')
    try {
      const parsed = new URL(`udp://${server}`)
      if (!parsed.hostname.startsWith('[')) throw new Error()
      return `${parsed.hostname}${parsed.port ? `:${parsed.port}` : ''}`
    } catch {
      throw new Error('Enter a STUN server as host[:port].')
    }
  }

  // The daemon accepts an unbracketed IPv6 address with its default port.
  // IPv6 plus an explicit port must use brackets to avoid ambiguity.
  if (validBareIPv6(server)) return server

  const match = /^([^:/?#@\s]+)(?::(\d{1,5}))?$/.exec(server)
  if (!match || !validPort(match[2]) || !validHostname(match[1])) throw new Error('Enter a STUN server as host[:port].')
  return `${match[1]}${match[2] ? `:${Number(match[2])}` : ''}`
}
