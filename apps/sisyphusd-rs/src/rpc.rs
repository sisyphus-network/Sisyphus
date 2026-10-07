use std::net::SocketAddr;
use std::pin::Pin;
use std::sync::Arc;

use anyhow::{Context, Result};
use futures_util::{Stream, stream};
use libp2p::{Multiaddr, PeerId, multiaddr::Protocol};
use tokio::{
    net::TcpListener,
    sync::{mpsc, oneshot, watch},
};
use tokio_stream::wrappers::TcpListenerStream;
use tonic::{Request, Response, Status, transport::Server};
use tracing::info;

use crate::storage::{BootstrapPeer, StateStore};

pub(crate) type ConnectCommand = (PeerId, Multiaddr, oneshot::Sender<Result<(), String>>);

pub(crate) mod proto {
    tonic::include_proto!("sisyphus.node.v1");
}

#[derive(Clone, Debug, Default)]
pub(crate) struct NodeSnapshot {
    pub(crate) peer_id: String,
    pub(crate) daemon_version: String,
    pub(crate) country_code: Option<String>,
    pub(crate) listen_addresses: Vec<String>,
    pub(crate) peers: Vec<PeerSnapshot>,
    pub(crate) peer_revision: u64,
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub(crate) struct PeerSnapshot {
    pub(crate) peer_id: String,
    pub(crate) connected: bool,
    pub(crate) known_addresses: Vec<String>,
    pub(crate) trusted_for_compute: bool,
}

pub(crate) struct RpcServer {
    address: SocketAddr,
    incoming: TcpListenerStream,
    state: watch::Receiver<NodeSnapshot>,
    store: Arc<dyn StateStore>,
    bootstrap_updates: watch::Sender<Vec<BootstrapPeer>>,
    connect_updates: mpsc::Sender<ConnectCommand>,
    trust_updates: mpsc::Sender<(PeerId, bool)>,
}

impl RpcServer {
    pub(crate) async fn bind(
        address: SocketAddr,
        state: watch::Receiver<NodeSnapshot>,
        store: Arc<dyn StateStore>,
        bootstrap_updates: watch::Sender<Vec<BootstrapPeer>>,
        connect_updates: mpsc::Sender<ConnectCommand>,
        trust_updates: mpsc::Sender<(PeerId, bool)>,
    ) -> Result<Self> {
        if !address.ip().is_loopback() {
            anyhow::bail!(
                "gRPC API can only bind to a loopback address until authentication is implemented"
            );
        }

        let listener = TcpListener::bind(address)
            .await
            .with_context(|| format!("failed to bind local gRPC API at {address}"))?;
        let address = listener
            .local_addr()
            .context("failed to inspect local gRPC API address")?;
        Ok(Self {
            address,
            incoming: TcpListenerStream::new(listener),
            state,
            store,
            bootstrap_updates,
            connect_updates,
            trust_updates,
        })
    }

    #[cfg(test)]
    fn local_addr(&self) -> SocketAddr {
        self.address
    }

    pub(crate) async fn serve(self, shutdown: oneshot::Receiver<()>) -> Result<()> {
        let service = NodeServiceImpl {
            state: self.state,
            store: self.store,
            bootstrap_updates: self.bootstrap_updates,
            connect_updates: self.connect_updates,
            trust_updates: self.trust_updates,
        };
        info!(address = %self.address, "local gRPC API listening");
        Server::builder()
            .add_service(proto::node_service_server::NodeServiceServer::new(service))
            .serve_with_incoming_shutdown(self.incoming, async {
                let _ = shutdown.await;
            })
            .await
            .with_context(|| format!("gRPC API server failed at {}", self.address))
    }
}

struct NodeServiceImpl {
    state: watch::Receiver<NodeSnapshot>,
    store: Arc<dyn StateStore>,
    bootstrap_updates: watch::Sender<Vec<BootstrapPeer>>,
    connect_updates: mpsc::Sender<ConnectCommand>,
    trust_updates: mpsc::Sender<(PeerId, bool)>,
}

#[tonic::async_trait]
impl proto::node_service_server::NodeService for NodeServiceImpl {
    type WatchPeersStream =
        Pin<Box<dyn Stream<Item = Result<proto::ListPeersResponse, Status>> + Send + 'static>>;

    async fn get_node_info(
        &self,
        _request: Request<proto::GetNodeInfoRequest>,
    ) -> Result<Response<proto::GetNodeInfoResponse>, Status> {
        let state = self.state.borrow().clone();
        Ok(Response::new(proto::GetNodeInfoResponse {
            peer_id: state.peer_id,
            daemon_version: state.daemon_version,
            listen_addresses: state.listen_addresses,
            country_code: state.country_code.unwrap_or_default(),
            // It runs no workloads.
            workloads: Vec::new(),
        }))
    }

    async fn list_peers(
        &self,
        _request: Request<proto::ListPeersRequest>,
    ) -> Result<Response<proto::ListPeersResponse>, Status> {
        let state = self.state.borrow().clone();
        let peers = state.peers;
        Ok(Response::new(proto::ListPeersResponse {
            peers: into_proto_peers(peers),
            revision: state.peer_revision,
        }))
    }

    async fn watch_peers(
        &self,
        _request: Request<proto::WatchPeersRequest>,
    ) -> Result<Response<Self::WatchPeersStream>, Status> {
        let stream = stream::unfold(
            (self.state.clone(), None),
            |(mut state, mut last_revision): (watch::Receiver<NodeSnapshot>, Option<u64>)| async move {
                loop {
                    if last_revision.is_some() && state.changed().await.is_err() {
                        return None;
                    }

                    let snapshot = state.borrow_and_update().clone();
                    if last_revision == Some(snapshot.peer_revision) {
                        continue;
                    }
                    last_revision = Some(snapshot.peer_revision);

                    let response = proto::ListPeersResponse {
                        peers: into_proto_peers(snapshot.peers),
                        revision: snapshot.peer_revision,
                    };
                    return Some((Ok(response), (state, last_revision)));
                }
            },
        );
        Ok(Response::new(Box::pin(stream)))
    }

    async fn get_bootstrap_peers(
        &self,
        _request: Request<proto::GetBootstrapPeersRequest>,
    ) -> Result<Response<proto::GetBootstrapPeersResponse>, Status> {
        let peers = self.store.load_bootstrap_peers().await.map_err(|error| {
            Status::internal(format!("failed to load bootstrap peers: {error}"))
        })?;
        Ok(Response::new(proto::GetBootstrapPeersResponse {
            peers: into_proto_bootstrap_peers(peers),
        }))
    }

    async fn set_bootstrap_peers(
        &self,
        request: Request<proto::SetBootstrapPeersRequest>,
    ) -> Result<Response<proto::SetBootstrapPeersResponse>, Status> {
        let peers = parse_bootstrap_peers(request.into_inner().peers)?;
        self.store
            .replace_bootstrap_peers(&peers)
            .await
            .map_err(|error| {
                Status::internal(format!("failed to save bootstrap peers: {error}"))
            })?;
        self.bootstrap_updates.send_replace(peers.clone());
        Ok(Response::new(proto::SetBootstrapPeersResponse {
            peers: into_proto_bootstrap_peers(peers),
        }))
    }

    async fn connect_peer(
        &self,
        request: Request<proto::ConnectPeerRequest>,
    ) -> Result<Response<proto::ConnectPeerResponse>, Status> {
        let address: Multiaddr = request.into_inner().address.parse().map_err(|error| {
            Status::invalid_argument(format!("invalid peer multiaddress: {error}"))
        })?;
        let peer_id = address
            .iter()
            .last()
            .and_then(|part| match part {
                Protocol::P2p(peer_id) => Some(peer_id),
                _ => None,
            })
            .ok_or_else(|| Status::invalid_argument("peer address must end with /p2p/<peer-id>"))?;
        let local_peer_id = self
            .state
            .borrow()
            .peer_id
            .parse::<PeerId>()
            .map_err(|_| Status::internal("local node has an invalid peer ID"))?;
        if peer_id == local_peer_id {
            return Err(Status::invalid_argument(
                "cannot connect to this node's own peer ID",
            ));
        }
        if self
            .state
            .borrow()
            .peers
            .iter()
            .any(|peer| peer.peer_id == peer_id.to_string() && peer.connected)
        {
            return Ok(Response::new(proto::ConnectPeerResponse {
                peer_id: peer_id.to_string(),
            }));
        }
        let (reply, result) = oneshot::channel();
        self.connect_updates
            .send((peer_id, address, reply))
            .await
            .map_err(|_| {
                Status::unavailable("networking loop is not accepting peer connections")
            })?;
        result
            .await
            .map_err(|_| Status::unavailable("networking loop stopped before dialing peer"))?
            .map_err(|error| {
                Status::unavailable(format!("failed to start peer connection: {error}"))
            })?;
        Ok(Response::new(proto::ConnectPeerResponse {
            peer_id: peer_id.to_string(),
        }))
    }

    async fn set_peer_compute_trust(
        &self,
        request: Request<proto::SetPeerComputeTrustRequest>,
    ) -> Result<Response<proto::SetPeerComputeTrustResponse>, Status> {
        let request = request.into_inner();
        let peer_id: PeerId = request
            .peer_id
            .parse()
            .map_err(|error| Status::invalid_argument(format!("invalid peer ID: {error}")))?;
        self.store
            .set_peer_compute_trust(peer_id, request.trusted)
            .await
            .map_err(|error| {
                Status::internal(format!("failed to update compute trust: {error}"))
            })?;
        self.trust_updates
            .send((peer_id, request.trusted))
            .await
            .map_err(|_| Status::unavailable("networking loop is not accepting trust updates"))?;
        Ok(Response::new(proto::SetPeerComputeTrustResponse {
            trusted: request.trusted,
        }))
    }

    async fn set_peer_compute_permissions(
        &self,
        _request: Request<proto::SetPeerComputePermissionsRequest>,
    ) -> Result<Response<proto::SetPeerComputePermissionsResponse>, Status> {
        // This daemon keeps one trust flag for each peer and runs no jobs.
        Err(Status::unimplemented(
            "this daemon keeps a single trust flag per peer; use SetPeerComputeTrust",
        ))
    }

    // This daemon runs no pool, so it has no workers or jobs. The Go daemon
    // serves these.
    type WatchJobsStream = Pin<Box<dyn Stream<Item = Result<proto::ListJobsResponse, Status>> + Send + 'static>>;

    async fn list_workers(
        &self,
        _request: Request<proto::ListWorkersRequest>,
    ) -> Result<Response<proto::ListWorkersResponse>, Status> {
        Err(Status::failed_precondition(NO_POOL))
    }

    async fn submit_job(
        &self,
        _request: Request<proto::SubmitJobRequest>,
    ) -> Result<Response<proto::SubmitJobResponse>, Status> {
        Err(Status::failed_precondition(NO_POOL))
    }

    async fn get_job(
        &self,
        _request: Request<proto::GetJobRequest>,
    ) -> Result<Response<proto::GetJobResponse>, Status> {
        Err(Status::failed_precondition(NO_POOL))
    }

    async fn list_jobs(
        &self,
        _request: Request<proto::ListJobsRequest>,
    ) -> Result<Response<proto::ListJobsResponse>, Status> {
        Err(Status::failed_precondition(NO_POOL))
    }

    async fn watch_jobs(
        &self,
        _request: Request<proto::WatchJobsRequest>,
    ) -> Result<Response<Self::WatchJobsStream>, Status> {
        Err(Status::failed_precondition(NO_POOL))
    }
}

const NO_POOL: &str = "this node coordinates no pool, so it has no workers or jobs of its own";

fn parse_bootstrap_peers(peers: Vec<proto::BootstrapPeer>) -> Result<Vec<BootstrapPeer>, Status> {
    let mut parsed = Vec::with_capacity(peers.len());
    for peer in peers {
        let peer_id: PeerId = peer
            .peer_id
            .parse()
            .map_err(|error| Status::invalid_argument(format!("invalid peer ID: {error}")))?;
        let address: Multiaddr = peer
            .address
            .parse()
            .map_err(|error| Status::invalid_argument(format!("invalid peer address: {error}")))?;
        match address.iter().last() {
            Some(Protocol::P2p(address_peer_id)) if address_peer_id == peer_id => {}
            Some(Protocol::P2p(address_peer_id)) => {
                return Err(Status::invalid_argument(format!(
                    "address peer ID {address_peer_id} does not match supplied peer ID {peer_id}"
                )));
            }
            _ => {
                return Err(Status::invalid_argument(
                    "bootstrap address must end with /p2p/<peer-id>",
                ));
            }
        }

        let entry = BootstrapPeer { peer_id, address };
        if !parsed.contains(&entry) {
            parsed.push(entry);
        }
    }
    Ok(parsed)
}

fn into_proto_bootstrap_peers(peers: Vec<BootstrapPeer>) -> Vec<proto::BootstrapPeer> {
    peers
        .into_iter()
        .map(|peer| proto::BootstrapPeer {
            peer_id: peer.peer_id.to_string(),
            address: peer.address.to_string(),
        })
        .collect()
}

fn into_proto_peers(peers: Vec<PeerSnapshot>) -> Vec<proto::Peer> {
    peers
        .into_iter()
        .map(|peer| proto::Peer {
            peer_id: peer.peer_id,
            connection_state: if peer.connected {
                proto::PeerConnectionState::Connected as i32
            } else {
                proto::PeerConnectionState::Disconnected as i32
            },
            known_addresses: peer.known_addresses,
            trusted_for_compute: peer.trusted_for_compute,
            // This daemon runs no jobs, so no work flows either way.
            works_for_this_node: false,
            this_node_works_for: false,
            // Its one flag stands for both sides.
            gives_work: peer.trusted_for_compute,
            takes_work: peer.trusted_for_compute,
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::{NodeServiceImpl, NodeSnapshot, PeerSnapshot, RpcServer, proto};
    use crate::storage::{BootstrapPeer, SqliteStore, StateStore};
    use libp2p::{Multiaddr, identity};
    use proto::{node_service_client::NodeServiceClient, node_service_server::NodeService};
    use std::sync::Arc;
    use tonic::Request;

    async fn test_storage(directory: &tempfile::TempDir) -> Arc<dyn StateStore> {
        Arc::new(
            SqliteStore::open(directory.path().join("node.sqlite3"))
                .await
                .expect("open test storage"),
        )
    }

    #[tokio::test]
    async fn node_service_exposes_current_node_and_peer_snapshot() {
        let snapshot = NodeSnapshot {
            peer_id: "node-peer-id".to_owned(),
            daemon_version: "0.1.0".to_owned(),
            country_code: Some("IL".to_owned()),
            listen_addresses: vec!["/ip4/127.0.0.1/tcp/7400".to_owned()],
            peers: vec![PeerSnapshot {
                peer_id: "worker-peer-id".to_owned(),
                connected: true,
                known_addresses: vec!["/ip4/127.0.0.1/tcp/7410".to_owned()],
                trusted_for_compute: false,
            }],
            peer_revision: 0,
        };
        let (_sender, receiver) = tokio::sync::watch::channel(snapshot);
        let directory = tempfile::tempdir().expect("temporary test directory");
        let store = test_storage(&directory).await;
        let (bootstrap_updates, _bootstrap_receiver) = tokio::sync::watch::channel(Vec::new());
        let (connect_updates, _connect_receiver) = tokio::sync::mpsc::channel(4);
        let (trust_updates, _trust_receiver) = tokio::sync::mpsc::channel(4);
        let service = NodeServiceImpl {
            state: receiver,
            store,
            bootstrap_updates,
            connect_updates,
            trust_updates,
        };

        let node = service
            .get_node_info(Request::new(proto::GetNodeInfoRequest {}))
            .await
            .expect("node info response")
            .into_inner();
        assert_eq!(node.peer_id, "node-peer-id");
        assert_eq!(node.daemon_version, "0.1.0");
        assert_eq!(node.country_code, "IL");

        let peers = service
            .list_peers(Request::new(proto::ListPeersRequest {}))
            .await
            .expect("peer list response")
            .into_inner();
        assert_eq!(peers.peers.len(), 1);
        assert_eq!(peers.peers[0].peer_id, "worker-peer-id");
        assert_eq!(
            peers.peers[0].connection_state,
            proto::PeerConnectionState::Connected as i32
        );
    }

    #[tokio::test]
    async fn generated_grpc_client_can_call_node_service() {
        let local_peer_id = identity::Keypair::generate_ed25519().public().to_peer_id();
        let snapshot = NodeSnapshot {
            peer_id: local_peer_id.to_string(),
            daemon_version: "0.1.0".to_owned(),
            country_code: Some("US".to_owned()),
            listen_addresses: vec!["/ip4/127.0.0.1/tcp/7400".to_owned()],
            peers: vec![PeerSnapshot {
                peer_id: "worker-peer-id".to_owned(),
                connected: false,
                known_addresses: vec!["/ip4/127.0.0.1/tcp/7410".to_owned()],
                trusted_for_compute: false,
            }],
            peer_revision: 0,
        };
        let (sender, receiver) = tokio::sync::watch::channel(snapshot.clone());
        let directory = tempfile::tempdir().expect("temporary test directory");
        let store = test_storage(&directory).await;
        let (bootstrap_updates, mut bootstrap_receiver) = tokio::sync::watch::channel(Vec::new());
        let (connect_updates, mut connect_receiver) = tokio::sync::mpsc::channel(4);
        let (trust_updates, mut trust_receiver) = tokio::sync::mpsc::channel(4);
        let server = RpcServer::bind(
            "127.0.0.1:0".parse().expect("loopback socket address"),
            receiver,
            store.clone(),
            bootstrap_updates,
            connect_updates,
            trust_updates,
        )
        .await
        .expect("bind API server");
        let address = server.local_addr();
        let (shutdown_sender, shutdown_receiver) = tokio::sync::oneshot::channel();
        let server_task = tokio::spawn(server.serve(shutdown_receiver));

        let mut client = NodeServiceClient::connect(format!("http://{address}"))
            .await
            .expect("connect generated gRPC client");
        let node = client
            .get_node_info(Request::new(proto::GetNodeInfoRequest {}))
            .await
            .expect("call GetNodeInfo")
            .into_inner();
        assert_eq!(node.peer_id, local_peer_id.to_string());
        assert_eq!(node.country_code, "US");

        let peers = client
            .list_peers(Request::new(proto::ListPeersRequest {}))
            .await
            .expect("call ListPeers")
            .into_inner();
        assert_eq!(peers.peers[0].peer_id, "worker-peer-id");
        assert_eq!(
            peers.peers[0].connection_state,
            proto::PeerConnectionState::Disconnected as i32
        );

        let mut updates = client
            .watch_peers(Request::new(proto::WatchPeersRequest {}))
            .await
            .expect("start peer watch")
            .into_inner();
        let initial = updates
            .message()
            .await
            .expect("read initial peer snapshot")
            .expect("initial peer snapshot exists");
        assert_eq!(initial.revision, 0);
        assert_eq!(
            initial.peers[0].connection_state,
            proto::PeerConnectionState::Disconnected as i32
        );

        let mut connected_snapshot = snapshot;
        connected_snapshot.peers[0].connected = true;
        connected_snapshot.peer_revision = 1;
        sender.send_replace(connected_snapshot);
        let update = tokio::time::timeout(std::time::Duration::from_secs(1), updates.message())
            .await
            .expect("peer update arrives promptly")
            .expect("read peer update")
            .expect("peer update exists");
        assert_eq!(update.revision, 1);
        assert_eq!(
            update.peers[0].connection_state,
            proto::PeerConnectionState::Connected as i32
        );

        let initial_bootstrap_peers = client
            .get_bootstrap_peers(Request::new(proto::GetBootstrapPeersRequest {}))
            .await
            .expect("call GetBootstrapPeers")
            .into_inner();
        assert!(initial_bootstrap_peers.peers.is_empty());

        let direct_peer_id = identity::Keypair::generate_ed25519().public().to_peer_id();
        let direct_address = format!("/ip4/127.0.0.1/tcp/7411/p2p/{direct_peer_id}");
        let mut direct_client = client.clone();
        let connect_task = tokio::spawn(async move {
            direct_client
                .connect_peer(Request::new(proto::ConnectPeerRequest {
                    address: direct_address,
                }))
                .await
        });
        let (requested_peer, requested_address, reply) = connect_receiver
            .recv()
            .await
            .expect("network receives direct connection request");
        assert_eq!(requested_peer, direct_peer_id);
        assert!(
            requested_address
                .to_string()
                .ends_with(&direct_peer_id.to_string())
        );
        reply.send(Ok(())).expect("return dial result");
        assert_eq!(
            connect_task.await.unwrap().unwrap().into_inner().peer_id,
            direct_peer_id.to_string()
        );

        client
            .set_peer_compute_trust(Request::new(proto::SetPeerComputeTrustRequest {
                peer_id: direct_peer_id.to_string(),
                trusted: true,
            }))
            .await
            .expect("set explicit compute trust");
        assert_eq!(
            trust_receiver.recv().await,
            Some((direct_peer_id, true)),
            "network receives explicit trust update"
        );
        assert!(
            store
                .load_compute_trusted_peers()
                .await
                .unwrap()
                .contains(&direct_peer_id)
        );

        let keypair = identity::Keypair::generate_ed25519();
        let peer_id = keypair.public().to_peer_id();
        let multiaddr: Multiaddr = format!("/ip4/127.0.0.1/tcp/7410/p2p/{peer_id}")
            .parse()
            .expect("valid bootstrap multiaddress");
        let setting = proto::BootstrapPeer {
            peer_id: peer_id.to_string(),
            address: multiaddr.to_string(),
        };
        let saved = client
            .set_bootstrap_peers(Request::new(proto::SetBootstrapPeersRequest {
                peers: vec![setting.clone()],
            }))
            .await
            .expect("call SetBootstrapPeers")
            .into_inner();
        assert_eq!(saved.peers, vec![setting.clone()]);
        bootstrap_receiver
            .changed()
            .await
            .expect("network receives bootstrap update");
        assert_eq!(
            *bootstrap_receiver.borrow_and_update(),
            vec![BootstrapPeer {
                peer_id,
                address: multiaddr.clone(),
            }]
        );

        let loaded = client
            .get_bootstrap_peers(Request::new(proto::GetBootstrapPeersRequest {}))
            .await
            .expect("call GetBootstrapPeers")
            .into_inner();
        assert_eq!(loaded.peers, vec![setting]);

        let mismatched_peer_id = identity::Keypair::generate_ed25519().public().to_peer_id();
        let invalid = client
            .set_bootstrap_peers(Request::new(proto::SetBootstrapPeersRequest {
                peers: vec![proto::BootstrapPeer {
                    peer_id: mismatched_peer_id.to_string(),
                    address: multiaddr.to_string(),
                }],
            }))
            .await
            .expect_err("reject peer ID/address mismatch");
        assert_eq!(invalid.code(), tonic::Code::InvalidArgument);

        shutdown_sender
            .send(())
            .expect("signal gRPC server shutdown");
        drop(sender);
        tokio::time::timeout(std::time::Duration::from_secs(2), server_task)
            .await
            .expect("gRPC server shuts down promptly")
            .expect("gRPC server task joins")
            .expect("gRPC server shuts down cleanly");
    }

    #[tokio::test]
    async fn api_server_only_binds_loopback_and_reports_bind_conflicts() {
        let directory = tempfile::tempdir().expect("temporary test directory");
        let store = test_storage(&directory).await;
        let (_state_sender, state_receiver) = tokio::sync::watch::channel(NodeSnapshot::default());
        let (bootstrap_sender, _bootstrap_receiver) = tokio::sync::watch::channel(Vec::new());
        let (connect_sender, _connect_receiver) = tokio::sync::mpsc::channel(4);
        let (trust_sender, _trust_receiver) = tokio::sync::mpsc::channel(4);

        let non_loopback = RpcServer::bind(
            "0.0.0.0:50051".parse().expect("socket address"),
            state_receiver.clone(),
            store.clone(),
            bootstrap_sender.clone(),
            connect_sender.clone(),
            trust_sender.clone(),
        )
        .await
        .err()
        .expect("non-loopback bind must be rejected");
        assert!(non_loopback.to_string().contains("loopback"));

        let occupied_listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind occupied socket");
        let occupied_address = occupied_listener.local_addr().expect("occupied address");
        let bind_error = RpcServer::bind(
            occupied_address,
            state_receiver,
            store,
            bootstrap_sender,
            connect_sender,
            trust_sender,
        )
        .await
        .err()
        .expect("conflicting bind must return an error");
        assert!(
            bind_error
                .to_string()
                .contains("failed to bind local gRPC API")
        );
    }
}
