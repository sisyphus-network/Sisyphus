mod cli;
mod geolocation;
mod networking;
mod observability;
mod platform;
mod rpc;
mod storage;

use clap::Parser;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observability::init();
    networking::run(cli::Args::parse()).await
}
