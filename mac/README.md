# 蜂信 Mac

完整源码及许可证见仓库 README、LICENSE 和 NOTICE.md。

在仓库根目录依次运行 `mac/build.command` 与 `mac/app/build-app.command`，输出在 `dist/Fengxin-Mac/`。默认需要 Apple Silicon、macOS 27、Xcode Command Line Tools、Go 1.26.3+ 和 Python 3.12+。

打开生成的 `蜂信.app`。保留整个交付目录以及同级 `EC20-Mac/data` 锁目录；不要只移动 App。构建创建的 Python 虚拟环境需要在原构建机器运行，跨机器分发应单独制作可移动运行时。

首次启动生成本机随机密码；号码、邮箱和授权码由本人填写。未连接设备时可以查看本地资料，真实通话和短信需要蜂窝注册，电脑通话还需要选择麦克风与扬声器。退出网关后自动恢复器也会停止。

USB 网卡模式见源码仓库 `docs/ECM.md`。不要将本机 config.json、data、logs、runtime.json 或手机SIP配置文件上传 GitHub。
