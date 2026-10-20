type Settings = { enabled: boolean; address: string; port: number }
type Address = { address: string; lan: boolean }

export const allInterfaces = '0.0.0.0'

// A form opened before any address was saved offers a LAN address first;
// all interfaces is never the one picked for the owner.
export function formForHosting(state: { saved: Settings; addresses: Address[] }): Settings {
  if (state.saved.address) return state.saved
  return { ...state.saved, address: state.addresses.find((entry) => entry.lan)?.address ?? state.addresses[0]?.address ?? '' }
}
