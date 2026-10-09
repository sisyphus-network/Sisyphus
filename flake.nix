{
  description = "Sisyphus Go daemon and Electron development environment";

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
              go_1_27
              gopls
              delve
              go-tools
              golangci-lint
              gnumake
              git
              protobuf
              protoc-gen-go
              protoc-gen-go-grpc
              kubo
              pkg-config
              nodejs_24
            ] ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [
              pkgs.gtk3
              pkgs.gsettings-desktop-schemas
            ];

            # Use the Nix-provided compiler rather than silently downloading
            # another toolchain. Keep this package aligned with go.mod.
            GOTOOLCHAIN = "local";

            shellHook = ''
              ${pkgs.lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
                export XDG_DATA_DIRS="${pkgs.gtk3}/share/gsettings-schemas/${pkgs.gtk3.name}:${pkgs.gsettings-desktop-schemas}/share/gsettings-schemas/${pkgs.gsettings-desktop-schemas.name}:''${XDG_DATA_DIRS:-}"
              ''}
              echo "Sisyphus development shell (Go + Electron/React)"
              echo "Go: $(go version)"
              echo "Protobuf: $(protoc --version)"
              echo "Node: $(node --version)"
            '';
          };
        });
    };
}
