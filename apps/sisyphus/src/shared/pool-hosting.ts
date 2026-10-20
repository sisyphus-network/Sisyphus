type Settings = { enabled: boolean; address: string; port: number }
type Address = { address: string; lan: boolean }

export const allInterfaces = '0.0.0.0'

// A form opened before any address was saved offers a LAN address first;
// all interfaces is never the one picked for the owner.
export function formForHosting(state: { saved: Settings; addresses: Address[] }): Settings {
  if (state.saved.address) return state.saved
  return { ...state.saved, address: state.addresses.find((entry) => entry.lan)?.address ?? state.addresses[0]?.address ?? '' }
}

// Where workers point --coordinator: the chosen address, or each of this
// computer's own when the node listens on all of them.
export function hostingJoinAddresses(state: { running: Settings; addresses: Address[] }): string[] {
  if (!state.running.enabled) return []
  const hosts = state.running.address === allInterfaces ? state.addresses.map((entry) => entry.address) : [state.running.address]
  return hosts.map((host) => `${host}:${state.running.port}`)
}

// Lets workers on a private network reach the pool port, and nothing else.
export function windowsFirewallCommand(port: number): string {
  return `netsh advfirewall firewall add rule name="Sisyphus pool (TCP ${port})" dir=in action=allow protocol=TCP localport=${port} profile=private`
}
