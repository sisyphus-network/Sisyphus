use std::path::PathBuf;

use anyhow::{Context, Result};
use directories::BaseDirs;

const APP_DIRECTORY: &str = "sisyphus";
const DATABASE_FILE: &str = "node.sqlite3";

/// Return the platform-appropriate local data directory for this node.
pub(crate) fn app_data_dir() -> Result<PathBuf> {
    let base_dirs = BaseDirs::new().context("could not locate the user's data directory")?;
    Ok(base_dirs.data_local_dir().join(APP_DIRECTORY))
}

pub(crate) fn database_path(data_dir: Option<PathBuf>) -> Result<PathBuf> {
    let data_dir = match data_dir {
        Some(path) => path,
        None => app_data_dir()?,
    };

    Ok(data_dir.join(DATABASE_FILE))
}
