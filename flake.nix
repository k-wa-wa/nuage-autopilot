{
  description = "nuage-autopilot: GitHub Issue/PR 駆動でアプリ開発を自動化するオートパイロットの Nix パッケージ";

  inputs = {
    # nuage-cluster/nix/flake.nix と同じ 24.11 に揃える。
    # autopilot は go.mod で最小バージョンのみを要求し、特定の新しい Go 機能に依存しない。
    # これにより nixpkgs 同梱の go をそのまま使え、Go 用の追加 input が不要になる。
    nixpkgs.url = "github:nixos/nixpkgs/nixos-24.11";
  };

  outputs =
    { self, nixpkgs }:
    let
      # nuage-cluster/nix/flake.nix の forAllSystems と同じ形（flake-utils は使わず、
      # 依存を増やさないために nixpkgs.lib.genAttrs で自前展開する）。
      systems = [ "x86_64-linux" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f system);
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          nuage-autopilot = pkgs.buildGoModule {
            pname = "nuage-autopilot";
            version = "0.1.0";

            src = ./.;

            ldflags = [ "-X main.version=0.1.0" ];

            # vendor/ をコミットする方針のため vendorHash は不要にする。
            # エージェント自身が依存を追加するため、vendorHash 管理を必須にすると
            # 依存追加のたびにハッシュがズレてビルドが壊れる。詳細は autopilot/DESIGN.md 5章。
            vendorHash = null;

            meta = {
              description = "GitHub Issue/PR を起点に自律型 LLM CLI を駆動するオートパイロット";
              mainProgram = "nuage-autopilot";
            };
          };

          default = self.packages.${system}.nuage-autopilot;
        }
      );
    };
}
