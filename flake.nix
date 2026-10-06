{
  description = "Sisyphus development environment";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { nixpkgs, ... }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = function:
        builtins.listToAttrs (map (system: {
          name = system;
          value = function system;
        }) systems);
    in
    {
      devShells = forAllSystems (system:
        let pkgs = import nixpkgs { inherit system; };
        in {
          default = pkgs.mkShell {
            packages = with pkgs; [
              go
              protobuf
              protoc-gen-go
              protoc-gen-go-grpc
              kubo
              nodejs_22
              gtk3
              gsettings-desktop-schemas
            ];

            shellHook = ''
              export XDG_DATA_DIRS="${pkgs.gtk3}/share/gsettings-schemas/${pkgs.gtk3.name}:${pkgs.gsettings-desktop-schemas}/share/gsettings-schemas/${pkgs.gsettings-desktop-schemas.name}:$XDG_DATA_DIRS"
              echo "Sisyphus development shell (Go + Electron/React)"
              echo "Go: $(go version)"
              echo "Protobuf: $(protoc --version)"
              echo "Node: $(node --version)"
            '';
          };
        });
    };
}
