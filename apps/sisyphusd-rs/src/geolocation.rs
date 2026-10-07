use std::time::Duration;

use serde::Deserialize;
use tracing::debug;

#[derive(Deserialize)]
struct GeoIpResponse {
    country_code: Option<String>,
}

/// Detect the country of the node's public egress IP without returning or storing that IP.
/// GeoIP is best-effort and must never prevent the daemon from starting.
pub(crate) async fn detect_country_code() -> Option<String> {
    let client = match reqwest::Client::builder()
        .timeout(Duration::from_secs(3))
        .user_agent(concat!("sisyphusd/", env!("CARGO_PKG_VERSION")))
        .build()
    {
        Ok(client) => client,
        Err(error) => {
            debug!(%error, "could not initialize GeoIP client");
            return None;
        }
    };

    let result = async {
        let response = client
            .get("https://ipapi.co/json/")
            .send()
            .await?
            .error_for_status()?;
        let location: GeoIpResponse = response.json().await?;
        Ok::<_, reqwest::Error>(location.country_code)
    }
    .await;

    match result {
        Ok(Some(country_code)) if is_iso_alpha2(&country_code) => {
            Some(country_code.to_ascii_uppercase())
        }
        Ok(_) => None,
        Err(error) => {
            debug!(%error, "GeoIP lookup unavailable; starting without a country code");
            None
        }
    }
}

fn is_iso_alpha2(value: &str) -> bool {
    value.len() == 2 && value.bytes().all(|byte| byte.is_ascii_alphabetic())
}

#[cfg(test)]
mod tests {
    use super::is_iso_alpha2;

    #[test]
    fn accepts_only_two_ascii_letters() {
        assert!(is_iso_alpha2("US"));
        assert!(is_iso_alpha2("il"));
        assert!(!is_iso_alpha2("USA"));
        assert!(!is_iso_alpha2("1L"));
        assert!(!is_iso_alpha2("ישראל"));
    }
}
