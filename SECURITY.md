# 安全

## 报告漏洞

请不要在公开的 issue 里描述还没修复的漏洞。用 GitHub 的私下报告：仓库的 **Security → Report a vulnerability**（<https://github.com/shakespark/vps-probe/security/advisories/new>）。

我会尽快确认收到，修复后在发布说明里说明并致谢（如果你愿意署名）。这是一个个人维护的项目，没有赏金。

## 哪些算漏洞

这个项目的安全模型写在 [README](README.md#安全模型) 和 [docs/DESIGN.md](docs/DESIGN.md) §2。打破其中任何一条的都算，例如：

- 让服务端（或任何网络上的人）在 agent 所在的机器上执行代码、写文件，或改变 agent 的行为；
- 不知道 token 而让服务端接受伪造的报文，或者冒充别的节点；
- 未登录而读到网页或接口的数据；经网页或接口改变服务端的数据或配置；
- 让节点上报的字符串在网页里执行脚本；
- 通过通知渠道向服务端下达指令；
- 伪造能通过签名验证的发布包。

不在范围内的：已经拿到服务端或某个节点 root 权限之后能做的事（见 README 里"如果这个被攻破"的表）；对公网 UDP 端口的洪水攻击。

## 支持的版本

只有最新的发布版本会得到修复。

## 验证发布包

每个版本都用离线密钥签名，验证方法见 [README](README.md#快速开始)。发布公钥的指纹：

```
SHA256:58Ji7UsUqJ+HoARS8RJsg88YH+M1bhivbOcJSXY/0Dg
```
