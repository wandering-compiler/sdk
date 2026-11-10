// The conformance service is built against the suite's own manifest (the
// image sets W17_BUSINESS_MANIFEST to the same file): an author's crate
// calls w17_business_build::compile(), which finds its manifest under the
// project's w17/services/.
fn main() -> Result<(), Box<dyn std::error::Error>> {
    let manifest = match std::env::var(w17_business_build::ENV_MANIFEST) {
        Ok(p) if !p.is_empty() => std::path::PathBuf::from(p),
        _ => {
            std::path::PathBuf::from(std::env::var("CARGO_MANIFEST_DIR")?).join("w17-business.json")
        }
    };
    w17_business_build::compile_manifest(&manifest, None, true)?;
    Ok(())
}
