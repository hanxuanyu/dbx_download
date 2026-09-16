# dbxdl

`dbxdl` 是 DBX 离线安装包的下载与打包工具。它会把 DBX 的多个 CPU 架构安装包和指定的官方插件
统一下载到一个目录中，压缩成一个 tar 包，然后**自动删除该目录，只保留 tar 包**。
打包前的目录结构与手工整理的示例完全一致。

下载并打包时的暂存目录结构（最外层是 `dbx<版本号>`）：

```
dbx0.6.14/                                        <- 暂存目录，打包成功后自动删除
├── DBX_0.6.14_arm64.dmg                          <- macOS (Apple Silicon)
├── DBX_0.6.14_x64-setup.exe                      <- Windows x64 安装程序
├── DBX_0.6.14_arm64-browser-static.tar.gz        <- Linux arm64 browser static
├── DBX_0.6.14_x64-portable.zip                   <- Windows x64 便携版
├── DBX_0.6.14_x64-browser-static.tar.gz          <- Linux x64 browser static
└── plugins/                                      <- 插件目录
    ├── s3/
    │   ├── io.github.t8y2.s3-0.1.9-linux-x64.dbxp
    │   ├── io.github.t8y2.s3-0.1.9-linux-arm64.dbxp
    │   └── io.github.t8y2.s3-0.1.9-windows-x64.dbxp
    └── ssh/
        ├── io.dbx.ssh-0.4.76-linux-x64.dbxp
        ├── io.dbx.ssh-0.4.76-linux-arm64.dbxp
        └── io.dbx.ssh-0.4.76-windows-x64.dbxp
```

打包完成后的输出目录（默认只留这两项）：

```
dbx0.6.14.tar.gz                                  <- 打包结果，内含 dbx0.6.14/ 完整目录树
dbx0.6.14.tar.gz.sha256                           <- 校验文件
```

`tar xzf dbx0.6.14.tar.gz` 解包后即可还原上面的目录结构。

## 构建

需要 Go 1.24 或更高版本。

```bash
make build            # 产出 bin/dbxdl
make install          # 安装到 $GOBIN
make test             # 运行全部单元测试
```

也可以直接运行：`go run . download`。

## 快速开始

```bash
# 1) 生成带注释的默认配置（可选，不生成也能用内置默认值）
./bin/dbxdl config init

# 2) 先看计划，不下载
./bin/dbxdl download --dry-run

# 3) 下载最新版并打包到当前目录
./bin/dbxdl download

# 4) 下载指定版本到 ./dist
./bin/dbxdl download --version 0.6.13 --out-dir ./dist
```

## 命令

| 命令 | 说明 |
| --- | --- |
| `dbxdl download [version]` | 下载指定版本（默认 `latest`）的 DBX 安装包与插件，并打包为 tar |
| `dbxdl latest` | `download --version latest` 的简写 |
| `dbxdl list` | 列出 GitHub 上最近的 DBX 版本 |
| `dbxdl plugins list` | 列出插件市场中的所有插件 |
| `dbxdl plugins show <id>` | 查看某个插件的全部版本、各平台产物与 sha256 |
| `dbxdl config init/show/path` | 生成 / 查看配置文件 |

### download 常用参数

```
--version, -V <v>     版本号，如 0.6.14 或 v0.6.14，默认 latest
--out-dir, -o <dir>   输出目录（覆盖 config 中的 output.dir）
--asset <selector>    追加需要下载的 release 资源（可重复，支持 * ? 与 {version}）
--plugin <id=ver>     临时指定某个插件的版本（可重复）
--plugin-latest       忽略配置里锁定的插件版本，全部取最新
--skip-plugins        只下载 DBX 安装包
--skip-dbx            只下载插件
--no-archive          只下载到目录，不打 tar 包（此时会保留目录，不会删除）
--format <fmt>        tar.gz（默认）或 tar
--keep-dir            保留打包用的 dbx<版本> 暂存目录（默认打包成功后删除）
--allow-missing       某个资源不存在时只告警不失败
--force               即使文件已存在且校验通过也重新下载
--concurrency <n>     并发下载数（默认取 config）
--retries <n>         单文件重试次数（默认取 config）
--dry-run             只打印计划
```

## 数据源

| 数据 | 来源 |
| --- | --- |
| DBX 版本列表 / 最新版本 | GitHub REST API：`GET /repos/t8y2/dbx/releases`，按 `github.tag_pattern` 过滤 |
| DBX 安装包文件 | GitHub Releases 的 `browser_download_url`（自动跟随 302 到 CDN） |
| DBX 安装包校验值 | Release 中附带的 `<asset>.sha256`（官方只为部分产物提供） |
| 插件元数据 | `https://raw.githubusercontent.com/t8y2/dbx-store/main/plugins/<插件ID>.json` |
| 插件文件 | 元数据中的 `artifacts[].url`（默认 `dl.dbxio.com`），或插件自身仓库的 release 资源 |
| 插件校验值 | 元数据中的 `artifacts[].sha256` |

> `t8y2/dbx` 同一个仓库里同时存在 `v0.6.x`、`packages-v0.4.x`、`agents-v0.2.x`
> 三套 tag。`github.tag_pattern`（默认 `^v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)$`）
> 只匹配 DBX 应用本体，避免 `latest` 取到 packages/agents 的版本。

### 插件下载来源：store 还是 GitHub

配置文件里的 `plugins.source` 控制插件包从哪里下载：

- `store`（默认）：使用 dbx-store 元数据中的 `url`，即 `dl.dbxio.com` 上带官方签名
  （`signingKeyId: dbx-store-release-2026`）的分发包。
- `github`：使用插件自身仓库的 release 资源。通过 GitHub API 定位对应版本的 release
  （元数据里的 `source` 字段可能过期，例如 s3 仍写着 `tree/v0.1.3` 而最新版已是 0.1.9，
  此时会自动改为遍历该仓库的 release 列表并按产物文件名匹配）。注意同一插件版本在两条链路上的
  **文件内容并不相同**（重新打包/签名导致大小与 sha256 不同）。如果需要复刻「插件仓库 release」
  那一份，就把 `plugins.source` 设为 `github`。
- `auto`：优先 `store`，元数据里没有该目标平台时回退到 `github`。

## 配置文件

`config.yaml` 由 `dbxdl config init` 生成，所有字段都有内置默认值，删掉也能正常运行。
关键字段：

```yaml
github:
  owner: t8y2
  repo: dbx
  tag_pattern: '^v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)$'
  include_prerelease: false
  token: ""                 # 建议改用环境变量 GITHUB_TOKEN

output:
  dir: "."                  # 输出根目录
  dir_name_template: "dbx{version}"
  keep_dir: false           # false = 打包成功后删除暂存目录，只留 tar
  concurrency: 4
  retries: 3
  verify_sha256: true

archive:
  enabled: true
  format: tar.gz            # tar.gz | tar
  name_template: "dbx{version}"
  gzip_level: 1             # 包内多为已压缩文件，默认取最快
  sha256_sidecar: true      # 生成 <包名>.sha256

assets:                     # 需要下载的 5 种包类型
  - selector: "DBX_{version}_arm64.dmg"
  - selector: "DBX_{version}_x64-setup.exe"
  - selector: "DBX_{version}_arm64-browser-static.tar.gz"
  - selector: "DBX_{version}_x64-portable.zip"
  - selector: "DBX_{version}_x64-browser-static.tar.gz"

plugins:
  enabled: true
  source: store             # store | github | auto
  root: "plugins"
  version_policy: pinned    # pinned = 用 item.version；latest = 全部取最新
  targets: [linux-x64, linux-arm64, windows-x64]
  items:
    - id: io.github.t8y2.s3
      version: "0.1.9"      # 留空表示取该插件最新版
      dir: s3               # 留空则直接用插件 ID 作为目录名
    - id: io.dbx.ssh
      version: "0.4.76"
      dir: ssh
```

`dir` 留空时不会做任何缩写推导，而是直接使用插件 ID，例如
`<版本目录>/plugins/io.github.t8y2.s3/io.github.t8y2.s3-0.1.9-linux-x64.dbxp`。

## 行为说明

- **增量与断点续传**：文件先下载到 `<文件>.part`，校验通过后再原子改名。中断后重跑会带着
  `Range` 请求从断点继续；已存在且大小/校验值正确的文件会直接跳过（显示 `==`）。
- **校验**：插件元数据全部带 sha256，一定校验；DBX 官方只为部分产物提供 `.sha256`，
  缺失时跳过并提示。校验失败的文件会被删除并报错，不会留下坏包。
- **清理暂存目录**：`output.keep_dir` 默认为 `false`，因此 tar 包写好后会自动删除
  `dbx<版本>/` 目录，只留下 tar 包与 `.sha256`。删除是 `RemoveAll`，所以删除前会逐项确认：

  1. tar 包存在且大小非空；
  2. tar 包内文件数不少于本次下载的文件数；
  3. 目录里没有本次下载之外的文件（`.DS_Store` 等系统文件与 `.part` 不算）。

  任何一条不满足都会**保留目录**并说明原因，避免 tar 出问题或目录里混有其它内容时被误删。
  用 `--keep-dir` 或 `keep_dir: true` 可始终保留目录；配合 `--no-archive` 时即使
  `keep_dir=false` 也会保留目录（否则会把唯一的下载成果删掉）。
- **打包**：tar 内条目以 `dbx<版本>/` 为前缀，解包后即为上面的目录结构；目录条目也会写入。
  归档时会自动跳过 `.DS_Store`、`Thumbs.db`、`._*` 等系统垃圾文件以及未完成的 `.part` 文件。
  通过固定 mtime、uid/gid 与不写入 atime/ctime，相同输入可以得到逐字节一致的 tar 包。
  条目时间戳取该 release 的发布日期；在限额降级模式下无法获知发布日期，会回退为 Unix epoch，
  因此降级模式产出的 tar 与正常模式不同（但同一种模式内部仍是完全可复现的）。
- **GitHub 限额**：匿名调用 API 为 60 次/小时。一次 `download` 只消耗 1~2 次（插件元数据走
  raw 内容，不计入限额；`plugins.source: github` 会额外消耗 1~2 次）。设置
  `GITHUB_TOKEN` / `GH_TOKEN` 可提升到 5000 次/小时。

  限额耗尽时的降级行为：
  - `dbxdl list` 自动回退到 releases.atom，只列版本号；
  - `dbxdl download` 从 Atom feed 取最新版本（或使用指定的 `--version`），并按
    `assets[].selector` 直接拼出下载地址（release 资源地址由 tag 与文件名唯一确定）。
    此时无法校验文件大小，`sha256` 仍会在官方提供了 `.sha256` 的情况下校验，输出中会明确标注
    `[degraded]`。若某个 selector 含 `*`/`?` 通配符则无法展开，会直接报错；需要完整校验或使用
    通配符时请配置 `GITHUB_TOKEN`。

## 在已有目录上运行（重要）

打包成功后 `dbx<版本>/` 会被**整目录删除**。因此在已经手工整理好 `dbx0.6.14/` 的目录里
直接运行 `dbxdl download`，会导致：

1. 校验不一致的插件包被重新下载覆盖（手工那份来自插件仓库 release，默认 `store` 源内容不同）；
2. 打包成功后该目录被整体删除，只剩 `dbx0.6.14.tar.gz`。

> 如果那个目录里只有本次要下载的文件，它会被删除；一旦里面还有其它文件（例如自己放的
> `notes.txt`、额外产物），工具会拒绝删除并列出这些文件。也就是说，想保留目录最省事的办法
> 就是别让它成为「纯暂存目录」，或者直接用 `--keep-dir`。

要保留手工整理的目录，请任选一种做法：

```bash
# 1) 输出到新目录，完全不动现有目录
./bin/dbxdl download --out-dir ./dist

# 2) 先预览会发生什么，不下载、不删除
./bin/dbxdl download --dry-run

# 3) 就地运行但保留目录
./bin/dbxdl download --keep-dir
```

`dbxdl` 本身是幂等的：已存在且（按大小与 sha256）校验通过的文件会直接复用并显示 `==`；
大小不匹配的文件会被重新下载覆盖。不过要注意，若打包后目录被删除，这个复用优势只在下一次
运行前仍然有效——所以需要反复增量下载时建议配合 `--keep-dir`。

如果要让下载内容与手工整理的那一份**逐字节一致**，请在配置中设置
`plugins.source: github`（插件包改为从插件自身仓库的 release 获取）。

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 成功 |
| 1 | 运行失败（网络、校验、配置错误等） |
| 2 | 命令行用法错误 |
| 130 | 被 Ctrl-C 中断（`.part` 文件保留，可续传） |
