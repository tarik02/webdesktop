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
            pkgs.libei
          ];
          webdesktop = buildGoModule {
            pname = "webdesktop";
            inherit version;

            src = pkgs.lib.cleanSource ./.;
            vendorHash = "sha256-x6H1qbLoOzRzpsS3yPQli8I7uUzQ58omr5oXzWxfTtI=";
            subPackages = [ "cmd/webdesktop" ];
            doCheck = false;

            nativeBuildInputs = mediaNativeBuildInputs ++ [ pkgs.makeWrapper ];
            buildInputs = mediaBuildInputs;

            ldflags = [
              "-s"
              "-w"
              "-X github.com/tarik02/webdesktop/internal/version.Version=${version}"
            ];

            postInstall = ''
              wrapProgram $out/bin/webdesktop \
                --set GST_PLUGIN_SYSTEM_PATH_1_0 "${gstPluginPath}" \
                --run 'export GST_REGISTRY_1_0="''${XDG_RUNTIME_DIR:-/tmp}/webdesktop-gstreamer-${pkgs.gst_all_1.gstreamer.version}-''${UID}.bin"'

              mkdir -p $out/lib/systemd/user $out/share/webdesktop
              substitute ${./packaging/systemd/webdesktop.service} \
                $out/lib/systemd/user/webdesktop.service \
                --replace-fail '@webdesktop@' "$out"
              install -m 0644 ${./webdesktop.example.yaml} \
                $out/share/webdesktop/config.example.yaml
            '';

            meta = with pkgs.lib; {
              description = "KDE Plasma Wayland desktop streaming service";
              mainProgram = "webdesktop";
              platforms = platforms.linux;
            };
          };
        in
        {
          packages.default = webdesktop;

          apps.default = {
            type = "app";
            program = "${webdesktop}/bin/webdesktop";
            meta.description = "Run the webdesktop service";
          };

          checks = {
            package = webdesktop;

            vet = buildGoModule {
              pname = "webdesktop-vet";
              inherit version;

              src = pkgs.lib.cleanSource ./.;
              vendorHash = "sha256-x6H1qbLoOzRzpsS3yPQli8I7uUzQ58omr5oXzWxfTtI=";
              doCheck = false;

              nativeBuildInputs = mediaNativeBuildInputs;
              buildInputs = mediaBuildInputs;

              buildPhase = ''
                runHook preBuild
                go vet ./...
                runHook postBuild
              '';
              installPhase = ''
                runHook preInstall
                mkdir -p $out
                touch $out/passed
                runHook postInstall
              '';
            };

            systemd-unit =
              pkgs.runCommand "webdesktop-systemd-unit"
                {
                  nativeBuildInputs = [ pkgs.systemd ];
                }
                ''
                  export HOME=$TMPDIR
                  export XDG_RUNTIME_DIR=$TMPDIR
                  export SYSTEMD_UNIT_PATH=${webdesktop}/lib/systemd/user:${pkgs.systemd}/example/systemd/user
                  systemd-analyze --user verify \
                    ${webdesktop}/lib/systemd/user/webdesktop.service
                  mkdir -p $out
                  touch $out/passed
                '';
          };

          devShells.default = pkgs.mkShell {
            nativeBuildInputs = mediaNativeBuildInputs;
            buildInputs = mediaBuildInputs;

            packages = [
              go
              pkgs.gopls
              pkgs.gotools
              pkgs.clang-tools
              pkgs.nixfmt
              pkgs.gst_all_1.gstreamer.bin
            ]
            ++ gstPlugins;

            GST_PLUGIN_SYSTEM_PATH_1_0 = gstPluginPath;
            CGO_ENABLED = "1";
          };

          formatter = pkgs.nixfmt;
        }
      );
}
