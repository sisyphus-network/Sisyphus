use std::{
    fs::{self, OpenOptions},
    path::{Path, PathBuf},
    time::Duration,
};

use anyhow::{Context, Result, bail};
use async_trait::async_trait;
use libp2p::identity;
use sqlx::{
    Row,
    sqlite::{SqliteConnectOptions, SqliteJournalMode, SqlitePoolOptions, SqliteSynchronous},
};
use tracing::info;

use super::{BootstrapPeer, StateStore};

static MIGRATOR: sqlx::migrate::Migrator = sqlx::migrate!("./migrations");

pub(crate) struct SqliteStore {
    pool: sqlx::SqlitePool,
}

impl SqliteStore {
    pub(crate) async fn open(database_path: PathBuf) -> Result<Self> {
        prepare_database_path(&database_path)?;

        let options = SqliteConnectOptions::new()
            .filename(&database_path)
            .create_if_missing(true)
            .foreign_keys(true)
            .journal_mode(SqliteJournalMode::Wal)
            .synchronous(SqliteSynchronous::Normal)
            .busy_timeout(Duration::from_secs(5));
        let pool = SqlitePoolOptions::new()
            .max_connections(4)
            .connect_with(options)
            .await
            .with_context(|| {
                format!(
                    "failed to open node database at {}",
                    database_path.display()
                )
            })?;

        MIGRATOR.run(&pool).await.with_context(|| {
            format!(
                "failed to migrate node database at {}",
                database_path.display()
            )
        })?;

        info!(database_path = %database_path.display(), "opened node state database");
        Ok(Self { pool })
    }
}

#[async_trait]
impl StateStore for SqliteStore {
    async fn load_or_create_identity(&self) -> Result<identity::Keypair> {
        if let Some(row) = sqlx::query(
            "SELECT encoding_version, private_key FROM node_identity WHERE singleton_id = 1",
        )
        .fetch_optional(&self.pool)
        .await?
        {
            return decode_identity(row.get::<i64, _>(0), row.get::<Vec<u8>, _>(1));
        }

        let candidate = identity::Keypair::generate_ed25519();
        let encoded = candidate.to_protobuf_encoding()?;
        sqlx::query(
            "INSERT OR IGNORE INTO node_identity (singleton_id, encoding_version, private_key) VALUES (1, 1, ?)",
        )
        .bind(encoded)
        .execute(&self.pool)
        .await
        .context("failed to persist node identity")?;

        let row = sqlx::query(
            "SELECT encoding_version, private_key FROM node_identity WHERE singleton_id = 1",
        )
        .fetch_one(&self.pool)
        .await
        .context("node identity was not present after initialization")?;

        decode_identity(row.get::<i64, _>(0), row.get::<Vec<u8>, _>(1))
    }

    async fn save_bootstrap_peer(&self, peer: &BootstrapPeer) -> Result<()> {
        sqlx::query("INSERT OR IGNORE INTO bootstrap_peers (peer_id, address) VALUES (?, ?)")
            .bind(peer.peer_id.to_string())
            .bind(peer.address.to_string())
            .execute(&self.pool)
            .await
            .context("failed to persist bootstrap peer")?;

        Ok(())
    }

    async fn replace_bootstrap_peers(&self, peers: &[BootstrapPeer]) -> Result<()> {
        let mut transaction = self
            .pool
            .begin()
            .await
            .context("failed to start bootstrap peer update")?;
        sqlx::query("DELETE FROM bootstrap_peers")
            .execute(&mut *transaction)
            .await
            .context("failed to clear existing bootstrap peers")?;

        for peer in peers {
            sqlx::query("INSERT OR IGNORE INTO bootstrap_peers (peer_id, address) VALUES (?, ?)")
                .bind(peer.peer_id.to_string())
                .bind(peer.address.to_string())
                .execute(&mut *transaction)
                .await
                .context("failed to save bootstrap peer")?;
        }

        transaction
            .commit()
            .await
            .context("failed to commit bootstrap peer update")
    }

    async fn clear_bootstrap_peers(&self) -> Result<()> {
        sqlx::query("DELETE FROM bootstrap_peers")
            .execute(&self.pool)
            .await
            .context("failed to clear bootstrap peers")?;

        Ok(())
    }

    async fn load_bootstrap_peers(&self) -> Result<Vec<BootstrapPeer>> {
        let rows =
            sqlx::query("SELECT peer_id, address FROM bootstrap_peers ORDER BY peer_id, address")
                .fetch_all(&self.pool)
                .await
                .context("failed to load bootstrap peers")?;

        rows.into_iter()
            .map(|row| {
                let peer_id_text: String = row.get(0);
                let address_text: String = row.get(1);
                let peer_id = peer_id_text.parse().with_context(|| {
                    format!("invalid peer ID in bootstrap peer store: {peer_id_text}")
                })?;
                let address = address_text.parse().with_context(|| {
                    format!("invalid multiaddress in bootstrap peer store: {address_text}")
                })?;
                Ok(BootstrapPeer { peer_id, address })
            })
            .collect()
    }

    async fn set_peer_compute_trust(&self, peer_id: libp2p::PeerId, trusted: bool) -> Result<()> {
        if trusted {
            sqlx::query("INSERT OR IGNORE INTO trusted_compute_peers (peer_id) VALUES (?)")
                .bind(peer_id.to_string())
                .execute(&self.pool)
                .await
                .context("failed to trust peer for compute")?;
        } else {
            sqlx::query("DELETE FROM trusted_compute_peers WHERE peer_id = ?")
                .bind(peer_id.to_string())
                .execute(&self.pool)
                .await
                .context("failed to revoke compute trust for peer")?;
        }
        Ok(())
    }

    async fn load_compute_trusted_peers(&self) -> Result<Vec<libp2p::PeerId>> {
        let peer_ids: Vec<String> =
            sqlx::query_scalar("SELECT peer_id FROM trusted_compute_peers ORDER BY peer_id")
                .fetch_all(&self.pool)
                .await
                .context("failed to load compute trusted peers")?;
        peer_ids
            .into_iter()
            .map(|peer_id| {
                peer_id
                    .parse()
                    .with_context(|| format!("invalid peer ID in compute trust store: {peer_id}"))
            })
            .collect()
    }
}

fn decode_identity(encoding_version: i64, encoded: Vec<u8>) -> Result<identity::Keypair> {
    if encoding_version != 1 {
        bail!("unsupported node identity encoding version: {encoding_version}");
    }

    identity::Keypair::from_protobuf_encoding(&encoded)
        .context("stored node identity is not a valid libp2p key")
}

fn prepare_database_path(database_path: &Path) -> Result<()> {
    let data_dir = database_path
        .parent()
        .context("node database path must have a parent directory")?;
    fs::create_dir_all(data_dir).with_context(|| {
        format!(
            "failed to create node data directory {}",
            data_dir.display()
        )
    })?;

    #[cfg(unix)]
    {
        use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};

        fs::set_permissions(data_dir, fs::Permissions::from_mode(0o700))
            .with_context(|| format!("failed to restrict permissions on {}", data_dir.display()))?;

        OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(false)
            .mode(0o600)
            .open(database_path)
            .with_context(|| {
                format!("failed to create node database {}", database_path.display())
            })?;
        fs::set_permissions(database_path, fs::Permissions::from_mode(0o600)).with_context(
            || {
                format!(
                    "failed to restrict permissions on {}",
                    database_path.display()
                )
            },
        )?;
    }

    #[cfg(not(unix))]
    {
        OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(false)
            .open(database_path)
            .with_context(|| {
                format!("failed to create node database {}", database_path.display())
            })?;
        // On Windows, the database inherits the current user's directory ACL.
    }

    Ok(())
}

#[cfg(test)]
mod tests {
    use crate::storage::BootstrapPeer;

    use super::{SqliteStore, StateStore};
    use libp2p::{Multiaddr, identity};

    #[tokio::test]
    async fn identity_survives_store_reopen() {
        let directory = tempfile::tempdir().expect("temporary data directory");
        let database_path = directory.path().join("node.sqlite3");

        let first_peer_id = {
            let store = SqliteStore::open(database_path.clone())
                .await
                .expect("open initial store");
            store
                .load_or_create_identity()
                .await
                .expect("create identity")
                .public()
                .to_peer_id()
        };

        let store = SqliteStore::open(database_path)
            .await
            .expect("reopen store");
        let second_peer_id = store
            .load_or_create_identity()
            .await
            .expect("load persisted identity")
            .public()
            .to_peer_id();

        assert_eq!(first_peer_id, second_peer_id);
    }

    #[tokio::test]
    async fn concurrent_initialization_keeps_one_identity() {
        let directory = tempfile::tempdir().expect("temporary data directory");
        let database_path = directory.path().join("node.sqlite3");
        let store_a = SqliteStore::open(database_path.clone())
            .await
            .expect("open first store");
        let store_b = SqliteStore::open(database_path)
            .await
            .expect("open second store");

        let (identity_a, identity_b) = tokio::join!(
            store_a.load_or_create_identity(),
            store_b.load_or_create_identity()
        );
        let peer_a = identity_a.expect("first identity").public().to_peer_id();
        let peer_b = identity_b.expect("second identity").public().to_peer_id();

        assert_eq!(peer_a, peer_b);
    }

    #[tokio::test]
    async fn bootstrap_peers_survive_store_reopen_and_are_deduplicated() {
        let directory = tempfile::tempdir().expect("temporary data directory");
        let database_path = directory.path().join("node.sqlite3");
        let keypair = identity::Keypair::generate_ed25519();
        let peer_id = keypair.public().to_peer_id();
        let address: Multiaddr = "/ip4/127.0.0.1/tcp/7400"
            .parse()
            .expect("valid test address");

        let store = SqliteStore::open(database_path.clone())
            .await
            .expect("open store");
        store
            .save_bootstrap_peer(&BootstrapPeer {
                peer_id,
                address: address.clone(),
            })
            .await
            .expect("save bootstrap peer");
        store
            .save_bootstrap_peer(&BootstrapPeer {
                peer_id,
                address: address.clone(),
            })
            .await
            .expect("save duplicate bootstrap peer");
        drop(store);

        let reopened = SqliteStore::open(database_path)
            .await
            .expect("reopen store");
        let peers = reopened
            .load_bootstrap_peers()
            .await
            .expect("load bootstrap peers");

        assert_eq!(peers, vec![BootstrapPeer { peer_id, address }]);

        reopened
            .clear_bootstrap_peers()
            .await
            .expect("clear bootstrap peers");
        assert!(reopened.load_bootstrap_peers().await.unwrap().is_empty());
    }

    #[test]
    fn encoded_identity_is_a_private_keypair() {
        let keypair = identity::Keypair::generate_ed25519();
        let encoded = keypair.to_protobuf_encoding().expect("encode identity");
        let decoded = identity::Keypair::from_protobuf_encoding(&encoded).expect("decode identity");

        assert_eq!(keypair.public().to_peer_id(), decoded.public().to_peer_id());
    }

    #[tokio::test]
    async fn compute_trust_survives_reopen_and_can_be_revoked() {
        let directory = tempfile::tempdir().expect("temporary data directory");
        let database_path = directory.path().join("node.sqlite3");
        let peer_id = identity::Keypair::generate_ed25519().public().to_peer_id();
        let store = SqliteStore::open(database_path.clone())
            .await
            .expect("open store");

        store
            .set_peer_compute_trust(peer_id, true)
            .await
            .expect("trust peer");
        let reopened = SqliteStore::open(database_path)
            .await
            .expect("reopen store");
        assert_eq!(
            reopened.load_compute_trusted_peers().await.unwrap(),
            vec![peer_id]
        );

        reopened
            .set_peer_compute_trust(peer_id, false)
            .await
            .expect("revoke trust");
        assert!(
            reopened
                .load_compute_trusted_peers()
                .await
                .unwrap()
                .is_empty()
        );
    }
}
