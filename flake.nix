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
          lib = pkgs.lib;
          go = pkgs.go_1_26;
          buildGoModule = pkgs.buildGoModule.override { inherit go; };
          nodejs = pkgs.nodejs_24;
          pnpm = pkgs.pnpm_11.override { nodejs-slim = pkgs.nodejs-slim_24; };
          version = self.shortRev or self.dirtyShortRev or "dev";
          repoRoot = toString ./.;
          source = lib.cleanSourceWith {
            src = ./.;
            filter =
              path: type:
              let
                relative = lib.removePrefix "${repoRoot}/" (toString path);
              in
              lib.cleanSourceFilter path type
              && relative != "web/node_modules"
              && !lib.hasPrefix "web/node_modules/" relative
              && relative != "web/dist"
              && !lib.hasPrefix "web/dist/" relative;
          };
          frontend = pkgs.stdenv.mkDerivation (finalAttrs: {
            pname = "webdesktop-web";
            inherit version;

            src = source;
            sourceRoot = "source/web";

            nativeBuildInputs = [
              nodejs
              pnpm
              pkgs.pnpmConfigHook
            ];

            pnpmDeps = pkgs.fetchPnpmDeps {
              inherit (finalAttrs)
                pname
                version
                src
                sourceRoot
                ;
              inherit pnpm;
              fetcherVersion = 4;
              hash = "sha256-RzyqL1hdpQixbE5xwopZaT2fQRhjYNxMHpXy12xrPEg=";
            };

            SSL_CERT_FILE = "${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt";

            buildPhase = ''
              runHook preBuild
              pnpm format:check
              pnpm lint
              pnpm typecheck
              pnpm build
              runHook postBuild
            '';

            installPhase = ''
              runHook preInstall
              mkdir -p $out
              cp -R dist/. $out/
              runHook postInstall
            '';
          });
          sourceWithFrontend = pkgs.runCommand "webdesktop-source-${version}" { } ''
            cp -R ${source}/. $out
            chmod -R u+w $out
            rm -rf $out/web/dist
            mkdir -p $out/web/dist
            cp -R ${frontend}/. $out/web/dist/
          '';
          gstPlugins = [
            pkgs.gst_all_1.gstreamer.out
            pkgs.gst_all_1.gst-plugins-base
            pkgs.gst_all_1.gst-plugins-bad
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

            src = sourceWithFrontend;
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

              mkdir -p $out/lib/systemd/user $out/share/applications $out/share/webdesktop
              substitute ${./packaging/systemd/webdesktop.service} \
                $out/lib/systemd/user/webdesktop.service \
                --replace-fail '@webdesktop@' "$out"
              substitute ${./packaging/applications/io.github.tarik02.webdesktop.desktop} \
                $out/share/applications/io.github.tarik02.webdesktop.desktop \
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
            frontend = frontend;

            vet = buildGoModule {
              pname = "webdesktop-vet";
              inherit version;

              src = sourceWithFrontend;
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

            desktop-entry =
              pkgs.runCommand "webdesktop-desktop-entry"
                {
                  nativeBuildInputs = [ pkgs.desktop-file-utils ];
                }
                ''
                  desktop-file-validate \
                    ${webdesktop}/share/applications/io.github.tarik02.webdesktop.desktop
                  grep -Fq \
                    "Exec=${webdesktop}/bin/webdesktop serve" \
                    ${webdesktop}/share/applications/io.github.tarik02.webdesktop.desktop
                  mkdir -p $out
                  touch $out/passed
                '';

            embedded-assets = pkgs.runCommand "webdesktop-embedded-assets" { } ''
              grep -aFq "Reconnect" ${webdesktop}/bin/.webdesktop-wrapped
              grep -aFq "/api/config" ${webdesktop}/bin/.webdesktop-wrapped
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
              nodejs
              pnpm
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
