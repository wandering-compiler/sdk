// Compiles the w17 vocabulary the SDK speaks itself (and maps an author's
// crate onto): every package under `w17` — ErrorDetail, Paging,
// InvalidateEvent, the options, the transaction coordinator, the companion's
// W17Files — from the committed `src/sdk_descriptors.binpb`
// (`make rs-descriptors`). No protoc: the descriptor set is the input.

use std::{env, fs, io, path::PathBuf};

use prost::Message;

fn main() -> io::Result<()> {
    let src =
        PathBuf::from(env::var("CARGO_MANIFEST_DIR").unwrap()).join("src/sdk_descriptors.binpb");
    println!("cargo:rerun-if-changed={}", src.display());
    let raw = fs::read(&src)?;
    let fds = prost_types::FileDescriptorSet::decode(raw.as_slice()).map_err(|e| {
        io::Error::new(
            io::ErrorKind::InvalidData,
            format!("{}: {e}", src.display()),
        )
    })?;
    // The coordinator's and the companion's servers are generated too: the
    // SDK's tests serve fakes of them.
    tonic_prost_build::configure()
        .build_client(true)
        .build_server(true)
        .build_transport(false)
        .include_file("w17_sdk_protos.rs")
        .compile_fds(fds)
}
