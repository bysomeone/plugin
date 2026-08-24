fn main() -> Result<(), Box<dyn std::error::Error>> {
    println!("cargo:rerun-if-changed=proto/rgb_sidecar.proto");
    tonic_prost_build::compile_protos("proto/rgb_sidecar.proto")?;
    Ok(())
}
