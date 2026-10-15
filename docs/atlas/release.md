---
kind: part
parent: README.md
code: [.github/workflows/, pkg/fileurl/service_release.go, pkg/fileurl/service_fetch.go, internal/task/task_check_version.go, internal/routers/api_router/handler_admin_control.go, internal/config/app.go, docker/, internal/app/version.go]
verified: 2026-10-15 @ 7eb516c9
---
# 发布与升级

> **30 秒版**：上游停更以后，fork 有自己的发行线。推一个 `v3.6.4` 这样的 tag，GitHub Actions 就测试、构建六个平台的程序包、发布 GHCR 容器镜像，最后发 Release。部署好的服务端检查更新、一键升级时，只认这个 fork 的 Release。

## 边界

负责：服务端的版本号、Release 和容器镜像的构建发布、服务端的版本检查和后台一键升级。

不负责：插件的发布（插件没有改动，直接用上游发布的版本，插件里的版本检查也看上游）；后台没有单独的发布，它随服务端发布；数据库结构升级（见[笔记库与存储](storage.md)）。

## 怎么运转

1. **打 tag。** 在要发布的提交上打 `v*` tag 并推送，触发 [fork-release.yml](../../.github/workflows/fork-release.yml)。
2. **构建。** 工作流先跑 `go test ./...`，再为 linux（amd64、arm64、armv7）、darwin（amd64、arm64）、windows（amd64）构建静态程序，打包成 `fast-note-sync-service-<tag>-<系统>-<架构>.tar.gz`。版本号从 tag 注入，不读 [version.go](../../internal/app/version.go) 里写的值。
3. **容器镜像和 Release。** 构建 amd64 和 arm64 的容器镜像，推到 `ghcr.io/liu-zhengdong/fast-note-sync-service`；容器镜像成功后才发 Release。Release 包和容器镜像来自同一个提交。
4. **检查更新。** 定时任务向本 fork 的 GitHub Release API 要版本列表（[task_check_version.go](../../internal/task/task_check_version.go)，地址在 [service_release.go](../../pkg/fileurl/service_release.go)）。`pull-source: github` 时只走这条直连。`auto`（默认）和 `cnb` 在直连失败（超时、5xx、限流）后，按 `github-proxies` 的前缀重试同一个 API，实际地址是「前缀 + 完整 API URL」。不读 CNB，也不回退到上游仓库。插件检查仍走上游，和这里无关。
5. **一键升级。** 管理员在后台点升级，服务端按 tag 和当前平台下载对应的包，校验通过后替换程序并重启（[handler_admin_control.go](../../internal/routers/api_router/handler_admin_control.go) 的 `Upgrade`）。这次下载用单独的时限，不受接口默认 60 秒上下文和 HTTP 写超时约束。`github` 模式只从 GitHub 直连下载。其他模式会并行拉取各源的 `SHA256SUMS`：能从 GitHub 拿到校验和就以它为准，否则采用多数代理一致的校验和；安装包按校验和响应的快慢依次下载，卡住或低于最低速度就换下一个源。校验和不匹配就丢掉这个包。全部失败则拒绝升级，不会去下载上游的程序。

新装有两种方式，都只从本仓库下载：一键安装脚本（[quest_install.sh](../../scripts/quest_install.sh)，从 master 取脚本、从 Release 取程序包，没有 CNB 镜像），或 Docker（`ghcr.io/liu-zhengdong/fast-note-sync-service`）。部署和切换容器镜像的步骤见运行手册 [fork-release](../runbook/fork-release.zh-CN.md)。

## 为什么这样设计

- **升级只认 fork**：fork 的功能上游没有，从上游升级等于把功能降级掉。
- **Go 的 module 路径不改**，仍是 `github.com/haierkeys/fast-note-sync-service`。它只是编译用的标识，和从哪里下载升级无关；保留它，合并上游代码时不用全局改 import。

## 坑

- **上游留下的工作流还在**：`release.yml`、`alpha-release.yml`、`mirror-to-cnb.yml`。它们都带 `github.actor == 'haierkeys'` 判断，在本仓库里只会显示为"已跳过"，不会真的发布。→ 在 Actions 页面看到它们被跳过是正常的。
- **第一次推送的 GHCR 包默认是私有的**。→ 部署机器拉镜像失败。→ 在 GitHub Packages 里把它设成公开，或者部署机器用带 `read:packages` 的凭据登录。
- **后台一键升级只换容器里的程序，不换容器镜像**。→ 之后重建容器，又回到容器镜像里的旧程序。→ 重建容器时用对应版本的新容器镜像。
- **两个服务端同时写同一个数据库会损坏数据**。→ 切换容器镜像时先停旧的、备份数据库，再起新的。
- **版本号接着上游往下编**：上游最后一个 tag 是 `3.6.1`（不带 `v`），本仓库从 `v3.6.2` 开始。不再跟上游，所以不用考虑撞号。
- **装了上游版本的服务端，一键升级只看上游**。→ 升不到本仓库的版本。→ 先用一键安装脚本或本仓库的容器镜像手动换一次，之后一键升级就走本仓库。
- **加速代理只负责传输。** 有 GitHub 的 `SHA256SUMS` 就以它为准。GitHub 完全不可达时，才采用互相一致的代理校验和；当时若只有一个源能给出校验和，对得上也会装，但这个源若同时改包和校验和则看不出来。→ 内置三个代理（`https://gh-proxy.com/`、`https://ghfast.top/`、`https://gh-proxy.org/`）是为了让多数时候能交叉核对。`app.github-proxies` 不写就用这份内置列表；写成空列表表示不用代理。`pull-source: github` 时这项不生效。
