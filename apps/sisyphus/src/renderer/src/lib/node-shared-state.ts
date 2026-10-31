export type SharedNodeStateEvent = {
  state?: { case?: string; value?: unknown } | string
  node?: unknown
}

// grpc-js with proto-loader's oneofs option exposes the selected oneof name
// separately and puts its payload on the matching event field. Connect clients
// instead keep the oneof case and payload together in `state`.
export function nodeInfoFromSharedStateEvent(event: SharedNodeStateEvent): unknown {
  const node = event.state && typeof event.state === 'object' && event.state.case === 'node'
    ? event.state.value
    : event.state === 'node' ? event.node : undefined
  return node && typeof node === 'object' ? (node as { info?: unknown }).info : undefined
}
