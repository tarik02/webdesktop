{
  description = "webdesktop HTTP service scaffold";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        go = pkgs.go_1_26;
        buildGoModule = pkgs.buildGoModule.override { inherit go; };
        version = self.shortRev or self.dirtyShortRev or "dev";
      in
      {
        packages.default = buildGoModule {
          pname = "webdesktop";
          inherit version;

          src = pkgs.lib.cleanSource ./.;
          vendorHash = "sha256-TSrB9KQR6QSthMgJ4xEK6CEYYroRnw8G0cZBVPjEk90=";
          subPackages = [ "cmd/webdesktop" ];

          ldflags = [
            "-s"
            "-w"
            "-X github.com/tarik02/webdesktop/internal/version.Version=${version}"
          ];

          meta = with pkgs.lib; {
            description = "webdesktop HTTP service scaffold";
            mainProgram = "webdesktop";
            platforms = platforms.linux;
          };
        };

        apps.default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/webdesktop";
        };

        devShells.default = pkgs.mkShell {
          packages = [
            go
            pkgs.gopls
            pkgs.gotools
            pkgs.nixfmt
          ];
        };

        formatter = pkgs.nixfmt;
      }
    );
}
