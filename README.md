# 蜂信 · Fengxin EC20 for Mac

使用 Quectel EC20 的 Mac 桌面通信应用。SwiftUI 原生界面连接本地 Go 网关，通过 USB 控制口收发短信、控制电话，并通过 USB Audio 处理通话音频。

**本项目公开源码，采用 PolyForm Noncommercial 1.0.0 非商业许可。** 底层包含 VoHive 衍生代码，保留原作者声明；蜂信是独立的 Mac 界面与适配版本，不是 VoCat 或 VoHive 官方发行版。请先阅读 [LICENSE](LICENSE) 与 [NOTICE.md](NOTICE.md)。

## 功能与状态

| 功能 | 说明 |
| --- | --- |
| Mac 桌面界面 | 总览、电话、短信、邮箱转发、设备与设置 |
| 电话 | 呼叫、接听、挂断、静音、通话状态；需要蜂窝注册和可用麦克风/扬声器 |
| 短信 | 单条 UCS-2 短信发送、接收归档、搜索、未读、回复和草稿 |
| 邮箱转发 | 将新收到的短信通过 SMTP 转发到自己配置的邮箱；本机加密保存授权码 |
| 连接恢复 | USB 重新枚举后有界接回；不会因无信号循环重启，不重放不明确的外发 |
| 手机 SIP | 可选的局域网 SIP/RTP 模式；默认使用 Mac 桌面通话模式 |
| ECM 网卡 | 适用固件可同时保留 ECM、AT 和 UAC 接口，见 [ECM 配置说明](docs/ECM.md) |

这是实验版本。已在 EC20-CE-HDLG 上确认 macOS ECM 网卡识别及 AT/UAC 接口并存；蜂窝入网、真实短信收件、接通后的双向音频和邮件实际入箱仍需在自己的设备上验收。识别出 USB 网卡不表示已经连接运营商网络。

## 从源码构建

当前构建目标为 **Apple Silicon / macOS 27**，其他 Mac 系统版本尚未验证。准备：

- Xcode Command Line Tools，提供 `xcrun`、Swift 编译器和 macOS SDK。
- Go 1.26.3 或更新版本，Python 3.12 或更新版本，Python 包安装需要网络。
- 支持语音及 USB Audio 的 EC20 模块、天线、可用 SIM 和适配载板。

克隆或下载本仓库后，在源码根目录执行：

```sh
mac/build.command
mac/app/build-app.command
```

应用生成在 `dist/Fengxin-Mac/蜂信.app`。构建过程使用仓库内 `.build/` 缓存和 `dist/` 交付目录，不依赖开发者的文档目录。可通过 `FENGXIN_GO_BINARY`、`FENGXIN_PYTHON_BINARY`、`FENGXIN_OUTPUT_DIR` 和 `FENGXIN_BUILD_DIR` 指定工具与输出位置。

`runtime/` 是在构建机器上创建的 Python 虚拟环境。它适合同机运行；向其他机器分发二进制时，需要另行打包可移动的 Python 运行时并完成签名与真机验证。本仓库不附带已登录、已配置凭据的应用包。

## 使用

1. 打开蜂信。未插 EC20 时也可以查看本地短信、编辑草稿，或在「邮箱转发」设置收件邮箱。连接模块前请退出其他占用 EC20 控制口的软件。
2. 在“设备与设置”确认 SIM 和蜂窝注册状态，选择并保存麦克风、扬声器。
3. 在电话或短信页输入自己的收件号码；应用不会预置测试号码或自动外发。
4. 邮箱转发按需在应用内配置，授权码只在本机填写。首次历史短信不批量重放，结果不明确的投递不自动重发。

管理网页仅监听 `127.0.0.1:7576`。第一次启动在本机生成随机网页和 SIP 密码。配置、短信、联系人、日志及运行状态均为本地文件，已由 `.gitignore` 排除。

[配置字段示例](examples/config.example.json) 中的密码刻意留空，仅供开发参考；正常使用直接启动应用，不必复制示例配置。

启动器也可手动使用：

```sh
dist/Fengxin-Mac/runtime/bin/python3 dist/Fengxin-Mac/launcher.py start --desktop --no-open
dist/Fengxin-Mac/runtime/bin/python3 dist/Fengxin-Mac/launcher.py status
dist/Fengxin-Mac/runtime/bin/python3 dist/Fengxin-Mac/launcher.py stop --desktop --no-open
```

可选手机 SIP 模式：先停止桌面网关，再运行不带 `--desktop` 的启动命令；仅新建的空配置默认进入 SIP 模式。已有桌面配置需要先备份，再由自己选择建立独立 SIP 配置。此模式生成的“手机SIP配置.txt”含本机密码，不应上传。

## 测试

Python 的恢复和 USB 管道测试使用合成设备，不接入真实模组：

```sh
python3 -m unittest discover -s mac/tests -v
```

Mac 原生界面可使用模拟设备状态测试拨号和发送按钮、草稿恢复，以及各页面独立刷新失败时的表现。请从仓库根目录执行：

```sh
mkdir -p .build
xcrun swiftc -swift-version 5 -D FENGXIN_MODEL_TESTS \
  mac/app/FengxinCore.swift mac/app/FengxinMac.swift mac/tests/FengxinOfflineTests.swift \
  -o .build/fengxin-offline-tests
.build/fengxin-offline-tests
```

在 Mac 上运行 Go 通知及入口测试时，必须启用隔离钥匙串测试标签：

```sh
mkdir -p .build/test-tmp
TMPDIR="$PWD/.build/test-tmp" go test -tags integrationtest ./...
```

不要将离线测试通过写成真实电话、短信或邮件已送达。提交问题时请先删除号码、短信正文、邮箱授权码、IMEI/IMSI/ICCID、配置和原始日志中的私人信息。

## 代码结构

- `mac/app/`：SwiftUI/AppKit 界面与图标源。
- `mac/`：启动器、USB/音频辅助进程、连接恢复器和构建脚本。
- `cmd/vohive-sip/`：本地管理 API、嵌入式网页和网关入口。
- `internal/`：AT、短信、电话、SIP、SMTP 和平台适配。
- `audio-helper/`：继承的 Windows 音频辅助源码；不属于本次 Mac 构建目标。

Go module 路径保留上游 `github.com/iniwex5/vohive`，以维持现有内部导入及来源关系。代码来源、第三方组件和许可见 [NOTICE.md](NOTICE.md)。
