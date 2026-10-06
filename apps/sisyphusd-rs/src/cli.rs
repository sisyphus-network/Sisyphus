use std::path::PathBuf;

use clap::Parser;

#[derive(Debug, Parser)]
#[command(name = "sisyphusd", about = "Sisyphus peer-to-peer node")]
pub(crate) struct Args {
    /// Multiaddresses to listen on. Can be supplied more than once.
    #[arg(
        long = "listen",
        default_values = [
            "/ip4/0.0.0.0/tcp/7400",
            "/ip4/0.0.0.0/udp/7401/quic-v1"
        ]
    )]
    pub(crate) listen: Vec<String>,

    /// Bootstrap peer multiaddress, including its /p2p/<peer-id>. Can be supplied more than once.
    #[arg(long = "bootstrap")]
    pub(crate) bootstrap_peers: Vec<String>,

    /// Remove all saved bootstrap peers before adding any supplied with --bootstrap.
    #[arg(long)]
    pub(crate) clear_bootstrap_peers: bool,

    /// Print the saved bootstrap peer address book and exit.
    #[arg(long)]
    pub(crate) list_bootstrap_peers: bool,

    /// Directory for this node's local SQLite database (defaults to the OS application data directory).
    #[arg(long)]
    pub(crate) data_dir: Option<PathBuf>,

    /// Loopback address for the local gRPC API (remote binding is disabled until authentication exists).
    #[arg(long, default_value = "127.0.0.1:50051")]
    pub(crate) api_listen: String,
}
