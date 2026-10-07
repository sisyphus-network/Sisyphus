mod swarm;

use anyhow::{Context, Result};
use futures_util::StreamExt;
use libp2p::{Multiaddr, PeerId, Swarm};
use std::{collections::BTreeMap, net::SocketAddr};
use tokio::sync::{mpsc, oneshot, watch};
use tracing::{info, warn};

use crate::{
    cli::Args,
    rpc::{self, NodeSnapshot, PeerSnapshot},
    storage::BootstrapPeer,
};

pub(crate) async fn run(args: Args) -> Result<()> {
    let data_dir = args.data_dir;
    let storage = crate::storage::open(data_dir).await?;

    if args.list_bootstrap_peers {
        for peer in storage.load_bootstrap_peers().await? {
            println!("{} {}", peer.peer_id, peer.address);
        }
        return Ok(());
    }

    let keypair = storage.load_or_create_identity().await?;
    let local_peer_id = keypair.public().to_peer_id();
    let mut swarm = swarm::build_swarm(keypair)?;
    let country_code = crate::geolocation::detect_country_code().await;
    if let Some(country_code) = &country_code {
        info!(%country_code, "detected approximate country from public egress IP");
    }
    let mut trusted_peers = storage
        .load_compute_trusted_peers()
        .await?
        .into_iter()
        .collect();

    let api_address: SocketAddr = args
        .api_listen
        .parse()
        .with_context(|| format!("invalid gRPC API listen address: {}", args.api_listen))?;
    if !api_address.ip().is_loopback() {
        anyhow::bail!(
            "gRPC API can only bind to a loopback address until authentication is implemented"
        );
    }

    if args.clear_bootstrap_peers {
        storage.clear_bootstrap_peers().await?;
        info!("cleared saved bootstrap peers");
    }

    for listen_address in args.listen {
        let address: Multiaddr = listen_address
            .parse()
            .with_context(|| format!("invalid listen multiaddress: {listen_address}"))?;
        swarm
            .listen_on(address.clone())
            .with_context(|| format!("failed to listen on {address}"))?;
    }

    for bootstrap_address in args.bootstrap_peers {
        let address: Multiaddr = bootstrap_address
            .parse()
            .with_context(|| format!("invalid bootstrap multiaddress: {bootstrap_address}"))?;
        let peer_id = swarm::peer_id_from_address(&address)
            .with_context(|| format!("bootstrap address must include /p2p/<peer-id>: {address}"))?;

        storage
            .save_bootstrap_peer(&BootstrapPeer { peer_id, address })
            .await?;
    }

    let bootstrap_peers = storage.load_bootstrap_peers().await?;
    let (bootstrap_sender, mut bootstrap_updates) = watch::channel(bootstrap_peers.clone());
    let mut known_peers = BTreeMap::new();
    for peer in &bootstrap_peers {
        let peer_id = peer.peer_id;
        let address = &peer.address;
        info!(%peer_id, %address, "dialing saved bootstrap peer");
        known_peers
            .entry(peer_id)
            .or_insert_with(Vec::new)
            .push(address.to_string());
        swarm::add_peer_address(&mut swarm, peer_id, address.clone());
        swarm
            .dial(address.clone())
            .with_context(|| format!("failed to dial saved bootstrap peer {address}"))?;
    }

    let (state_sender, state_receiver) = watch::channel(NodeSnapshot {
        peer_id: local_peer_id.to_string(),
        daemon_version: env!("CARGO_PKG_VERSION").to_owned(),
        country_code: country_code.clone(),
        ..NodeSnapshot::default()
    });
    let (connect_sender, mut connect_receiver) = mpsc::channel::<rpc::ConnectCommand>(32);
    let (trust_sender, mut trust_receiver) = mpsc::channel::<(PeerId, bool)>(32);
    publish_snapshot(
        &swarm,
        local_peer_id,
        &known_peers,
        &trusted_peers,
        &state_sender,
    );
    let rpc_server = rpc::RpcServer::bind(
        api_address,
        state_receiver,
        storage,
        bootstrap_sender,
        connect_sender,
        trust_sender,
    )
    .await?;
    info!(%local_peer_id, "Sisyphus node started");
    let (rpc_shutdown, rpc_shutdown_receiver) = oneshot::channel();
    let rpc_server = rpc_server.serve(rpc_shutdown_receiver);
    tokio::pin!(rpc_server);

    let shutdown = tokio::signal::ctrl_c();
    tokio::pin!(shutdown);

    loop {
        tokio::select! {
            event = swarm.select_next_some() => {
                swarm::handle_swarm_event(event, &mut swarm, local_peer_id);
                publish_snapshot(&swarm, local_peer_id, &known_peers, &trusted_peers, &state_sender);
            },
            command = connect_receiver.recv() => {
                if let Some(command) = command {
                    let (peer_id, address, reply) = command;
                    swarm::add_peer_address(&mut swarm, peer_id, address.clone());
                    let result = swarm.dial(address.clone()).map_err(|error| error.to_string());
                    if result.is_ok() {
                        known_peers.entry(peer_id).or_insert_with(Vec::new).push(address.to_string());
                    }
                    let _ = reply.send(result);
                    publish_snapshot(&swarm, local_peer_id, &known_peers, &trusted_peers, &state_sender);
                }
            },
            update = trust_receiver.recv() => {
                if let Some((peer_id, trusted)) = update {
                    if trusted { trusted_peers.insert(peer_id); } else { trusted_peers.remove(&peer_id); }
                    publish_snapshot(&swarm, local_peer_id, &known_peers, &trusted_peers, &state_sender);
                }
            },
            result = bootstrap_updates.changed() => {
                result.context("bootstrap peer update channel closed")?;
                let peers = bootstrap_updates.borrow_and_update().clone();
                known_peers = address_book(&peers);
                for peer in peers {
                    if !swarm.is_connected(&peer.peer_id) {
                        swarm::add_peer_address(&mut swarm, peer.peer_id, peer.address.clone());
                        if let Err(error) = swarm.dial(peer.address.clone()) {
                            warn!(peer_id = %peer.peer_id, address = %peer.address, %error, "could not dial updated bootstrap peer");
                        }
                    }
                }
                publish_snapshot(&swarm, local_peer_id, &known_peers, &trusted_peers, &state_sender);
            },
            result = &mut rpc_server => {
                result?;
                break;
            }
            result = &mut shutdown => {
                result.context("failed to listen for shutdown signal")?;
                info!("shutdown requested");
                break;
            }
        }
    }

    let _ = rpc_shutdown.send(());
    drop(state_sender);
    rpc_server.await?;
    Ok(())
}

fn address_book(peers: &[BootstrapPeer]) -> BTreeMap<PeerId, Vec<String>> {
    let mut address_book = BTreeMap::new();
    for peer in peers {
        address_book
            .entry(peer.peer_id)
            .or_insert_with(Vec::new)
            .push(peer.address.to_string());
    }
    address_book
}

fn publish_snapshot(
    swarm: &Swarm<swarm::SisyphusBehaviour>,
    local_peer_id: PeerId,
    known_peers: &BTreeMap<PeerId, Vec<String>>,
    trusted_peers: &std::collections::BTreeSet<PeerId>,
    state_sender: &watch::Sender<NodeSnapshot>,
) {
    let mut peers: BTreeMap<PeerId, PeerSnapshot> = known_peers
        .iter()
        .map(|(peer_id, addresses)| {
            (
                *peer_id,
                PeerSnapshot {
                    peer_id: peer_id.to_string(),
                    connected: false,
                    known_addresses: addresses.clone(),
                    trusted_for_compute: trusted_peers.contains(peer_id),
                },
            )
        })
        .collect();

    for peer_id in swarm.connected_peers() {
        peers
            .entry(*peer_id)
            .and_modify(|peer| peer.connected = true)
            .or_insert_with(|| PeerSnapshot {
                peer_id: peer_id.to_string(),
                connected: true,
                known_addresses: Vec::new(),
                trusted_for_compute: trusted_peers.contains(peer_id),
            });
    }

    let peers: Vec<_> = peers.into_values().collect();
    let current = state_sender.borrow().clone();
    let peer_revision = if peers == current.peers {
        current.peer_revision
    } else {
        current.peer_revision.saturating_add(1)
    };

    state_sender.send_replace(NodeSnapshot {
        peer_id: local_peer_id.to_string(),
        daemon_version: env!("CARGO_PKG_VERSION").to_owned(),
        country_code: current.country_code.clone(),
        listen_addresses: swarm
            .listeners()
            .map(|address| {
                address
                    .clone()
                    .with(libp2p::multiaddr::Protocol::P2p(local_peer_id))
                    .to_string()
            })
            .collect(),
        peers,
        peer_revision,
    });
}
