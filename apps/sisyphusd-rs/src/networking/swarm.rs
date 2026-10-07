use std::time::Duration;

use anyhow::{Context, Result};
use libp2p::{
    Multiaddr, PeerId, Swarm, SwarmBuilder, identify, identity,
    kad::{self, store::MemoryStore},
    mdns, noise, ping,
    swarm::{NetworkBehaviour, SwarmEvent},
    tcp, yamux,
};
use tracing::{debug, info, warn};

const IDENTIFY_PROTOCOL: &str = "/sisyphus/node/1.0.0";

#[derive(NetworkBehaviour)]
pub(super) struct SisyphusBehaviour {
    identify: identify::Behaviour,
    ping: ping::Behaviour,
    mdns: mdns::tokio::Behaviour,
    kademlia: kad::Behaviour<MemoryStore>,
}

pub(super) fn build_swarm(keypair: identity::Keypair) -> Result<Swarm<SisyphusBehaviour>> {
    let swarm = SwarmBuilder::with_existing_identity(keypair)
        .with_tokio()
        .with_tcp(
            tcp::Config::default(),
            noise::Config::new,
            yamux::Config::default,
        )
        .context("failed to configure TCP transport")?
        .with_quic()
        .with_behaviour(|key| {
            let peer_id = key.public().to_peer_id();
            let identify = identify::Behaviour::new(identify::Config::new(
                IDENTIFY_PROTOCOL.to_owned(),
                key.public(),
            ));
            let ping =
                ping::Behaviour::new(ping::Config::new().with_interval(Duration::from_secs(15)));
            let mdns = mdns::tokio::Behaviour::new(mdns::Config::default(), peer_id)
                .expect("failed to initialize mDNS discovery");
            let mut kademlia = kad::Behaviour::new(peer_id, MemoryStore::new(peer_id));
            kademlia.set_mode(Some(kad::Mode::Server));

            SisyphusBehaviour {
                identify,
                ping,
                mdns,
                kademlia,
            }
        })
        .context("failed to configure libp2p behaviours")?
        .build();

    Ok(swarm)
}

pub(super) fn handle_swarm_event(
    event: SwarmEvent<SisyphusBehaviourEvent>,
    swarm: &mut Swarm<SisyphusBehaviour>,
    local_peer_id: PeerId,
) {
    match event {
        SwarmEvent::NewListenAddr { address, .. } => {
            info!(address = %address.with(libp2p::multiaddr::Protocol::P2p(local_peer_id)), "listening");
        }
        SwarmEvent::ConnectionEstablished {
            peer_id, endpoint, ..
        } => {
            info!(%peer_id, ?endpoint, "peer connection established");
        }
        SwarmEvent::ConnectionClosed { peer_id, cause, .. } => {
            info!(%peer_id, ?cause, "peer connection closed");
        }
        SwarmEvent::Behaviour(SisyphusBehaviourEvent::Mdns(mdns::Event::Discovered(peers))) => {
            for (peer_id, address) in peers {
                info!(%peer_id, %address, "discovered peer on local network");
                let address = with_peer_id(address, peer_id);
                swarm
                    .behaviour_mut()
                    .kademlia
                    .add_address(&peer_id, address.clone());
                if let Err(error) = swarm.dial(address.clone()) {
                    debug!(%peer_id, %address, %error, "could not dial discovered peer");
                }
            }
        }
        SwarmEvent::Behaviour(SisyphusBehaviourEvent::Mdns(mdns::Event::Expired(peers))) => {
            for (peer_id, address) in peers {
                debug!(%peer_id, %address, "local-network peer advertisement expired");
            }
        }
        SwarmEvent::Behaviour(SisyphusBehaviourEvent::Identify(identify::Event::Received {
            peer_id,
            info,
            ..
        })) => {
            info!(%peer_id, agent = %info.agent_version, protocol = %info.protocol_version, "identified peer");
            for address in info.listen_addrs {
                swarm
                    .behaviour_mut()
                    .kademlia
                    .add_address(&peer_id, with_peer_id(address, peer_id));
            }
            if let Err(error) = swarm.behaviour_mut().kademlia.bootstrap() {
                debug!(%peer_id, %error, "DHT bootstrap is waiting for known peers");
            }
        }
        SwarmEvent::Behaviour(SisyphusBehaviourEvent::Ping(ping::Event {
            peer, result, ..
        })) => match result {
            Ok(rtt) => debug!(%peer, ?rtt, "peer ping succeeded"),
            Err(error) => warn!(%peer, %error, "peer ping failed"),
        },
        SwarmEvent::Behaviour(SisyphusBehaviourEvent::Kademlia(event)) => {
            debug!(?event, "DHT event");
        }
        _ => {}
    }
}

pub(super) fn peer_id_from_address(address: &Multiaddr) -> Option<PeerId> {
    address.iter().find_map(|protocol| match protocol {
        libp2p::multiaddr::Protocol::P2p(peer_id) => Some(peer_id),
        _ => None,
    })
}

pub(super) fn add_peer_address(
    swarm: &mut Swarm<SisyphusBehaviour>,
    peer_id: PeerId,
    address: Multiaddr,
) {
    swarm
        .behaviour_mut()
        .kademlia
        .add_address(&peer_id, address);
}

fn with_peer_id(address: Multiaddr, peer_id: PeerId) -> Multiaddr {
    let already_qualified = matches!(
        address.iter().last(),
        Some(libp2p::multiaddr::Protocol::P2p(address_peer_id)) if address_peer_id == peer_id
    );

    if already_qualified {
        address
    } else {
        address.with(libp2p::multiaddr::Protocol::P2p(peer_id))
    }
}
