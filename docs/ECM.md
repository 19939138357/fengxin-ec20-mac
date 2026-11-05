# EC20 同时保留网卡、电话短信控制口与 USB 音频

在适用固件上，EC20 可以作为 USB 复合设备，分别提供 AT 控制、ECM 网卡与 UAC 音频接口。蜂信通过 AT 控制电话和短信，macOS 系统驱动接管 ECM；改变数据接口不等于接入运营商网络。

本项目的实测设备为 EC20-CE-HDLG、固件 EC20CEHDLGR08A03M1G。该固件 `usbnet=0` 为原 QMI 模式，`usbnet=1` 可枚举 ECM。切换后 macOS 的 AppleUserECM 驱动实际创建了 EC20-CE-HDLG 网络接口；AT 接口 2 和 UAC 音频仍存在。其他 EC20 版本应先核对自己的型号、固件与厂商手册，不能直接套用全部参数。

## 显式切换

先停止蜂信及其他占用 AT 口的程序，确认没有正在进行的电话。在独占的 AT 工具里读取并保存原始值：

```text
ATI
AT+QCFG="usbnet"
AT+QCFG="usbcfg"
AT+CLCC
```

仅在确认固件含义和支持范围后，修改数据模式并重启一次：

```text
AT+QCFG="usbnet",1
AT+CFUN=1,1
```

等设备重新枚举后，重新连接并读取 `AT+QCFG="usbnet"`、`AT+QCFG="usbcfg"`，检查 AT 和音频接口确实保留。不要更改未知的 USBCFG 标志，不需要刷固件或修改 IMEI。

macOS 可用 `networksetup -listallhardwareports` 查找 **EC20-CE-HDLG**，具体 `enX` 名称由系统分配；`networksetup -listnetworkserviceorder` 与 `route -n get default` 用于检查服务顺序和默认路由。

新 ECM 服务可能自动排在 Wi-Fi 前面。模组未入网时，即使 DHCP 分配了局域网地址，也可能没有外网出口；应在 macOS 中保留原有可用网络优先级。蜂信启动不会自动更改系统网络服务顺序。

## 分别验收

- 网卡：系统出现目标硬件接口、驱动已绑定。
- 蜂窝：SIM READY、CREG/CEREG 已注册；CSQ 99 表示未知信号读数。
- 上网：运营商 APN/PDP 已连接后，经该网卡实际访问外网。
- 电话短信：在蜂信内主动执行，分别核对来电、对端接通、双向音频与短信收件。

网卡出现在系统里，不代表以上各项已通过。ECM 的数据功能也不会把模块转换成 DJI 官方模块或自动接入 DJI 图传服务。

如需恢复原数据模式，应按已保存的原始值操作；本项目实测的原值为 0。USB/命令结果不明确时先重新读取，不自动重放重启、电话或短信。

参考：[Quectel EC20 ECM 配置说明](https://forums.quectel.com/t/hello-experts-please-let-me-ask-how-to-use-ec20-in-ecm/58876)。部分 EC20 固件不支持 QNETDEVCTL，因此本说明不将它作为必需步骤。
