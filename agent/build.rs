fn main() -> Result<(), Box<dyn std::error::Error>> {
    tonic_build::configure().compile_protos(&["../proto/agent/v1/agent.proto"], &["../proto"])?;

    // One version for the whole product, from the VERSION file at the root of
    // the repository. SN_AGENT_VERSION overrides it, for building the "next"
    // release in the self-update test without touching the file.
    println!("cargo:rerun-if-changed=../VERSION");
    println!("cargo:rerun-if-env-changed=SN_AGENT_VERSION");
    let version = std::env::var("SN_AGENT_VERSION")
        .ok()
        .filter(|v| !v.trim().is_empty())
        .or_else(|| std::fs::read_to_string("../VERSION").ok())
        .map(|v| v.trim().to_string())
        .filter(|v| !v.is_empty())
        .unwrap_or_else(|| std::env::var("CARGO_PKG_VERSION").unwrap());
    println!("cargo:rustc-env=SN_AGENT_VERSION={version}");
    Ok(())
}
