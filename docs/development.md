# 开发

设计和代码结构见 [DESIGN.md](DESIGN.md)（§5 是各个包的分工）。

## 构建与测试

需要 Go（版本见 `go.mod`；本机版本不同时 Makefile 会自动下载指定的工具链）。

```sh
make test          # go vet + 单元测试
make build         # dist/ 下的六个程序（三个程序 × amd64 / arm64）
make dist          # 发布包：dist/vps-probe-<版本>-linux-{amd64,arm64}.tar.gz 和 .sha256
make demo-check    # 演示站的数据和接口对得上（需要 node）
```

改了 `proto/` 要重新生成代码（需要 `protoc` 和 `protoc-gen-go`）：`make proto`。生成的代码已提交。

升级或增减 Go 依赖、更换 `web/static/vendor/` 里的前端库后执行 `make licenses`，提交更新后的 `THIRD_PARTY_LICENSES`。

## 在本机跑起来

不用 root，也不碰系统目录：

```sh
make build
cd dist
cat > server.yml <<EOT
listen: {web: "127.0.0.1:18080", ingest: "127.0.0.1:19527"}
public_addr: 127.0.0.1:19527
db: $PWD/probe.db
backup: {keep: 0}
nodes: []
EOT
chmod 640 server.yml
./vps-probe-server-linux-amd64 add-node -config server.yml -id local > /dev/null
./vps-probe-server-linux-amd64 agent-config -config server.yml -node local -o agent.yml
./vps-probe-server-linux-amd64 run -config server.yml &
mkdir -p state && STATE_DIRECTORY=$PWD/state ./vps-probe-agent-linux-amd64 run -config agent.yml
```

浏览器打开 <http://127.0.0.1:18080>（没有配登录保护，只监听本机）。

只看 agent 采到了什么、不发送：`vps-probe-agent run -config agent.yml -dry-run`。它把每份报文以 JSON 打印出来，不写流量状态文件，可以和已安装的 agent 同时运行。

## 网页

`web/static/` 是原生 ES 模块，没有构建步骤，改了刷新即可（要重新 `go build` 服务端，文件是嵌进程序里的）。`js/` 是各页共用的，`pages/` 每页一个模块。

改了接口的返回字段：

```sh
go test ./internal/server/api -update-shape    # 更新 web/demo/api-shape.json
make demo-check                                # 再改 web/demo/demo.js 直到它通过
```

## 演示站

`make demo` 生成 `dist/demo/`：同一份界面，加上在浏览器里生成假数据的 `web/demo/demo.js`，没有后端。整个目录放到静态托管的站点根路径即可（如 Cloudflare Pages：`npx wrangler pages deploy dist/demo`）。

## 发布

1. 改 `VERSION` 和 README 快速开始里的 `V=`（CI 会检查两者一致），提交，`git push origin main`；再 `git tag v<版本> && git push origin v<版本>`（tag 单独推送，和提交一起推可能不触发 CI）。
2. GitHub Actions 测试、构建，建一个草稿 Release。
3. 在自己的终端执行 `make release-sign`：核对草稿里的包与本地重建逐字节一致，签名，上传签名并正式发布。只核对不签名用 `make release-verify`。签名密钥默认是 `~/.ssh/vps-probe-release`，可用 `SIGNING_KEY=` 指定。

发布前在一台带 systemd 的干净机器（或容器）上用发布包实际走一遍安装：服务端、`add-node`、用打印的命令装 agent、升级、卸载。
