{
  description = "webdesktop KDE Wayland streaming service";

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
    flake-utils.lib.eachSystem
      [
        "x86_64-linux"
        "aarch64-linux"
      ]
      (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          go = pkgs.go_1_26;
          buildGoModule = pkgs.buildGoModule.override { inherit go; };
          version = self.shortRev or self.dirtyShortRev or "dev";
          gstPlugins = [
            pkgs.gst_all_1.gstreamer.out
            pkgs.gst_all_1.gst-plugins-base
            pkgs.gst_all_1.gst-plugins-good
            pkgs.gst_all_1.gst-plugins-ugly
            pkgs.pipewire
          ];
          gstPluginPath = pkgs.lib.makeSearchPath "lib/gstreamer-1.0" gstPlugins;
          mediaNativeBuildInputs = [
            pkgs.pkg-config
          ];
          mediaBuildInputs = [
            pkgs.gst_all_1.gstreamer
            pkgs.gst_all_1.gst-plugins-base
          ];
        in
        {
          packages.default = buildGoModule {
            pname = "webdesktop";
            inherit version;

            src = pkgs.lib.cleanSource ./.;
            vendorHash = "sha256-x6H1qbLoOzRzpsS3yPQli8I7uUzQ58omr5oXzWxfTtI=";
            subPackages = [ "cmd/webdesktop" ];

            nativeBuildInputs = mediaNativeBuildInputs ++ [ pkgs.makeWrapper ];
            buildInputs = mediaBuildInputs;

            ldflags = [
              "-s"
              "-w"
              "-X github.com/tarik02/webdesktop/internal/version.Version=${version}"
            ];

            postInstall = ''
              wrapProgram $out/bin/webdesktop \
                --set GST_PLUGIN_SYSTEM_PATH_1_0 "${gstPluginPath}"
            '';

            meta = with pkgs.lib; {
              description = "KDE Plasma Wayland desktop streaming service";
              mainProgram = "webdesktop";
              platforms = platforms.linux;
            };
          };

          apps.default = {
            type = "app";
            program = "${self.packages.${system}.default}/bin/webdesktop";
          };

          devShells.default = pkgs.mkShell {
            nativeBuildInputs = mediaNativeBuildInputs;
            buildInputs = mediaBuildInputs;

            packages = [
              go
              pkgs.gopls
              pkgs.gotools
              pkgs.nixfmt
              pkgs.gst_all_1.gstreamer.bin
            ]
            ++ gstPlugins;

            GST_PLUGIN_SYSTEM_PATH_1_0 = gstPluginPath;
          };

          formatter = pkgs.nixfmt;
        }
      );
}
