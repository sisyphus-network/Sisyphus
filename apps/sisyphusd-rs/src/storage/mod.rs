mod sqlite;

use std::{path::PathBuf, sync::Arc};

use anyhow::Result;
use async_trait::async_trait;
use libp2p::{Multiaddr, PeerId, identity};

use crate::platform;

pub(crate) use sqlite::SqliteStore;

#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct BootstrapPeer {
    pub(crate) peer_id: PeerId,
    pub(crate) address: Multiaddr,
}

/// Durable, node-local state. Network peers never share this store.
#[async_trait]
pub(crate) trait StateStore: Send + Sync {
    async fn load_or_create_identity(&self) -> Result<identity::Keypair>;
    async fn clear_bootstrap_peers(&self) -> Result<()>;
    async fn save_bootstrap_peer(&self, peer: &BootstrapPeer) -> Result<()>;
    async fn replace_bootstrap_peers(&self, peers: &[BootstrapPeer]) -> Result<()>;
    async fn load_bootstrap_peers(&self) -> Result<Vec<BootstrapPeer>>;
    async fn set_peer_compute_trust(&self, peer_id: PeerId, trusted: bool) -> Result<()>;
    async fn load_compute_trusted_peers(&self) -> Result<Vec<PeerId>>;
}

pub(crate) async fn open(data_dir: Option<PathBuf>) -> Result<Arc<dyn StateStore>> {
    let database_path = platform::database_path(data_dir)?;
    Ok(Arc::new(SqliteStore::open(database_path).await?))
}
