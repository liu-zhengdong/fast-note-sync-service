# 自有发行与线上升级

服务端版本检查及后台一键升级只使用 `liu-zhengdong/fast-note-sync-service` 的 GitHub Releases。`pull-source` 为 `auto`（默认）时，直连失败或下载过慢会改用 `github-proxies` 里的加速前缀访问同一份发布文件，并用该 Release 的 `SHA256SUMS` 校验后才替换程序。`pull-source: github` 只走直连。插件版本检查仍使用插件上游。不会回退到官方服务程序。升级下载不受接口默认 60 秒超时限制。

发布一个 `v3.6.2` 形式的 tag 会触发 `.github/workflows/fork-release.yml`：测试、构建六个平台的静态程序包、发布 amd64/arm64 GHCR 镜像，镜像成功后才发布 Release。更新包保留原始 tag：`fast-note-sync-service-v3.6.2-linux-amd64.tar.gz`。Release 包和 Docker 使用相同提交。

镜像名称固定为 `ghcr.io/liu-zhengdong/fast-note-sync-service`。首次 GHCR 包默认可能私有，需在 GitHub Packages 设置中公开，或部署主机使用拥有 read:packages 的凭据登录。后续发布使用仓库自带 GITHUB_TOKEN，无需 Docker Hub 或 CNB 凭据。

线上配置及数据挂载不变。`docker/compose.yml` 提供固定版本示例；实际部署宜固定镜像 digest，保留旧容器及旧镜像。先拉取并用隔离数据验证新镜像，最后停旧服务、完成数据库备份、启动新容器。单机单实例切换会短暂断开连接，Obsidian 会重新连接；禁止两个服务同时写同一数据库。

后台一键升级继续支持在容器内替换程序并重启进程。它不会改变 Docker 镜像标签；重建容器时必须使用对应的新镜像，避免回退到镜像的旧程序。Docker entrypoint 直接 exec 服务，使服务收到停止信号并能够完成优雅关闭。

不需要修改 Go module/import 路径：它是编译依赖标识，与升级仓库和镜像来源无关。上游作者署名仍保留。
