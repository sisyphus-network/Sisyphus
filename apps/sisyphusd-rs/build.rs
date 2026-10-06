fn main() {
    println!("cargo:rerun-if-changed=migrations");
    println!("cargo:rerun-if-changed=../../proto/sisyphus/node/v1/node.proto");

    tonic_prost_build::configure()
        .compile_protos(
            &["../../proto/sisyphus/node/v1/node.proto"],
            &["../../proto"],
        )
        .expect("failed to compile daemon protobuf definitions");
}
