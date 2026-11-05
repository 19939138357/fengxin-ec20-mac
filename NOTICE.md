# 蜂信 · 组件来源与许可

蜂信是本项目的 Mac 桌面应用名称，采用新原生界面、联系人、草稿、短信搜索和设备状态流程；不是 VoCat 或 VoHive 的官方发布。底层仍包含 VoHive 衍生代码，不宣称已从零重写。

基础源码来自 https://github.com/jikdarren/vohive commit 35ba2a238ed918155d5adaa23f16a13b2f44e79b，以及后续 Windows/Mac 适配。原作者的 Required Notice 保留如下。

Required Notice: Copyright iniwex5 (https://github.com/iniwex5/vohive)

保留 PolyForm Noncommercial License 1.0.0，完整条款见 LICENSE。改名不改变所继承代码的许可条件；本版按个人非商业用途交付。

Mac 使用 SwiftUI/AppKit、本地 Go 网关、Python、PyUSB/libusb、sounddevice/PortAudio 与 CoreAudio。源码发布不捆绑依赖运行时；通过 Go modules 和 mac/requirements.txt 安装依赖。分发构建产物时也须保留所包含依赖的许可文件。Windows 源码中使用的 NAudio 1.10.0 为 MIT 许可，Mac 不使用该 Windows 音频助手。二维码实现的许可保留在 cmd/vohive-sip/LICENSE.go-qrcode。

本仓库是在此来源基础上整理的当前源码快照，不包含开发者的本机运行配置、历史短信、联系人、凭据、诊断日志或构建缓存。
