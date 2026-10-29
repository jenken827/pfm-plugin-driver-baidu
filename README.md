# pfm-plugin-driver-baidu

百度网盘存储驱动，[pfm](https://github.com/jenken827/pfm) 的官方插件（独立进程）。
部分方法的实现参考了[OpenList](https://github.com/OpenListTeam/OpenList)

## 这是什么

[pfm](https://github.com/jenken827/pfm) 核心只内置 `local`；其余网盘以**独立进程插件**提供，通过 loopback RPC 与宿主通信
（协议定义在 `pfm-plugin-sdk` 仓库的 `proto/plugin` 与 `proto/abi`，SDK 侧实现在 `plugin/sdk`）。

⚠️ 插件与宿主**同权限**运行：能读宿主的 `config.yaml`（含全部 token）与 `data/`。
只安装官方发布或自己构建的二进制。

## 授权（没有 Client ID / Client Secret 也能用）

百度网盘 OpenAPI 的正常玩法是自己到[百度开放平台](https://pan.baidu.com/union/console/applist)
建应用，拿 `Client ID` + `Client Secret`，再走 OAuth 换 `refresh_token`。**没有这些凭据的用户
可以直接用 OpenList 的公开授权系统：<https://api.oplist.org/>** —— 在页面上选百度网盘、扫码授权，
把得到的 `refresh_token` 填进 [pfm](https://github.com/jenken827/pfm) 即可，插件会通过 OpenList 的续期接口自动刷新令牌。

驱动有两条续期路径，二选一（元数据里的「续期服务地址」决定走哪条）：

| 配置 | 续期方式 | 需要什么 |
|---|---|---|
| **续期服务地址非空**（默认 `https://api.oplist.org/baiduyun/renewapi`） | OpenList 公共授权：`GET renewapi?refresh_ui=…&server_use=true&driver_txt=baiduyun_go` | 只要 `refresh_token` |
| 续期服务地址留空 | 百度官方 OAuth2：`openapi.baidu.com/oauth/2.0/token` | `refresh_token` + 自有 `Client ID/Secret` |

两种方式的 `refresh_token` 都会在续期后**轮换并自动回写**到 provider 配置，不需要手工更新。
切换方式（尤其是从公共授权切到自有应用）后请重新填一次 `refresh_token`：令牌绑定了签发方的应用。

使用 OpenList 公共授权的最小配置：

| 字段 | 值 |
|---|---|
| Refresh Token | <https://api.oplist.org/> 上百度网盘授权后得到的 `refresh_token` |
| 续期服务地址 | 保持默认 `https://api.oplist.org/baiduyun/renewapi` |
| Client ID / Client Secret | **留空** |
| 根目录路径 | 按需（默认 `/`） |

> 公共授权是第三方公共服务，可用性、频率限制与隐私边界由 OpenList 决定；介意的话就自己建应用。
> 插件只把 `refresh_token` 发给该续期地址，不发送其他配置。

### 关于配置与数据定义

本插件不 import 宿主仓库的任何代码，只依赖插件 SDK（`github.com/jenken827/pfm-plugin-sdk`）：
宿主的内部数据模型怎么变都不会牵动它，插件契约（ABI）变了才会。

驱动已迁出为独立插件，`storage_providers.config` 里的结构**由本插件自己定义**
（`driver/baidu/config.go`），宿主只把它当作不透明 JSON 字符串存取。JSON 键名与元数据里
`ConfigField.Name` 逐字一致（前端表单按字段名拼 JSON），因此：

- 加/改一个配置项只需要改本仓库（`config.go` + `init.go` 的字段声明），不用动宿主的 proto；
- 配置 JSON 里多出来的旧键会被忽略，升级不会因为宿主 proto 变动而失败。

日志走 stderr，由宿主逐行转发进宿主日志（`[plugin baidu stderr] ...`）；级别可用环境变量
`PFM_PLUGIN_LOG_LEVEL`（`debug` / `info` / `warn` / `error`，默认 `info`）调整。

## 构建

依赖 `github.com/jenken827/pfm-plugin-sdk`，版本写在 `go.mod` 里，按 tag 从 Go module 代理取：

```bash
go build -trimpath -o pfm-plugin-driver-baidu .
```

要同时改 SDK 时用工作区把两个目录并起来，`go.work` 不入库：

```bash
go work init . ../pfm-plugin-sdk
```

插件与 SDK 的协议版本必须一致：SDK 的 `plugin/sdk.AbiVersion` 变了就要重编插件，
宿主加载时也会比对，不一致会直接报「协议版本 N 与宿主 M 不兼容」。

## 安装

目录布局（放到 [pfm](https://github.com/jenken827/pfm) 的 `plugin.dir` 下）：

```
plugins/pfm-plugin-driver-baidu/
  plugin.json
  pfm-plugin-driver-baidu
```

然后打开配置：

```yaml
plugin:
  enabled: true
```

或从官方索引安装：`pfm plugin install pfm-plugin-driver-baidu`。

## 发布

推送 tag 即自动构建并发布（`.github/workflows/release.yml`）：

```bash
# 1) 先把 plugin.json 的 version 改成要发的版本
# 2) 打 tag 并推送（tag 必须等于 v<version>，否则工作流直接失败）
git tag v1.0.0
git push origin v1.0.0
```

工作流交叉编译 `linux-amd64` / `linux-arm64` / `darwin-amd64` / `darwin-arm64` /
`windows-amd64`，把 `pfm-plugin-driver-baidu-<version>-<platform>.tar.gz`、`checksums.txt`
与 `index-entries.json` 作为资产发布到该 tag 的 GitHub Release。Release 说明由提交历史生成：
`上一个 tag..本 tag` 范围内的提交标题（首个版本列全部提交），也就是「这版改了什么」。
失败可重跑（同名资产覆盖、说明重写、Release 重新标为已发布，不会重复建）。

发布后把 `index-entries.json` 里的条目并进 `pfm-plugins-index` 的 `index.json`（`seq` 递增），
在索引仓库重新签名——整份索引共用一个单调递增的 `seq`，所以签名不在本仓库的 CI 里做。
条目里的 `url` / `sha256` 已经按 Release 资产算好，贴过去即可。

CI 只要 Go：SDK 按 `go.mod` 里的 tag 从 module 代理取，不需要检出宿主或 SDK 的源码，
Go 版本也直接读 `go.mod`，不在工作流里写死。插件与宿主之间的 ABI 版本由 `plugin.json` 的
`abi_version` 声明。

分支与 PR 的常规检查（编译 / vet / gofmt / 测试 / `plugin.json` 字段校验）在
`.github/workflows/ci.yml`。

### 没有 CI 时的手工发布

```bash
# 当前平台打包 + 生成签名索引（私钥只在发布机上使用，不入仓库）
scripts/make-release.sh --version 1.0.0 --seq 1 \
  --repo <owner>/pfm-plugin-driver-baidu \
  --base-url <包所在地址> --key <索引签名私钥>

# 或只为多平台包生成索引条目
scripts/package.sh --version 1.0.0 --goos linux --goarch arm64 --out dist
scripts/make-index-entries.sh --version 1.0.0 --tag v1.0.0 \
  --repo <owner>/pfm-plugin-driver-baidu --dir dist
```

包名、可执行文件名、`kind` / `driver_id` 这些字段都从 `plugin.json` 取，脚本里没有第二处定义。

## 许可

与 [pfm 主仓库](https://github.com/jenken827/pfm) 一致（AGPL-3.0）。分发二进制时须同时提供对应源码。
