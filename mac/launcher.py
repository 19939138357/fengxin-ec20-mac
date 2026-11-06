#!/usr/bin/env python3
"""Start/stop the local gateway and maintain private connection instructions."""
import base64
import fcntl
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
from device_supervisor import managed_active, private_write, read_session

BASE = Path(__file__).resolve().parent
os.umask(0o077)
CONFIG = BASE / 'config.json'


def write_private(path, text):
    tmp = path.with_suffix(path.suffix + '.new')
    tmp.write_text(text, encoding='utf-8')
    tmp.chmod(0o600)
    tmp.replace(path)


def lan_ip():
    # Prefer the local Wi-Fi interface, never a tunnel/public IP.
    import ipaddress
    for interface in ('en1', 'en0', 'en8'):
        r = subprocess.run(['/usr/sbin/ipconfig', 'getifaddr', interface], capture_output=True, text=True)
        value = r.stdout.strip()
        try:
            address = ipaddress.ip_address(value)
            if address.version == 4 and address.is_private and not address.is_link_local and not address.is_loopback:
                return value
        except ValueError:
            pass
    raise RuntimeError('请先让 Mac 连接手机所在的 Wi-Fi，再启动网关。')


def request(cfg, path='/api/status', body=None):
    address = 'http://' + cfg['web_listen']
    auth = base64.b64encode(('admin:' + cfg['web_password']).encode()).decode()
    headers = {'Authorization': 'Basic ' + auth}
    if body is not None:
        headers['Origin'] = address
        headers['Content-Type'] = 'application/json'
    req = urllib.request.Request(address + path, headers=headers,
                                 data=None if body is None else json.dumps(body).encode())
    try:
        with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req, timeout=3) as response:
            raw = response.read()
    except urllib.error.HTTPError as error:
        # A server answered. Treating e.g. a 401 as a connection failure
        # would start a competing gateway against the same port and modem.
        error.close()
        if error.code in (401, 403):
            raise RuntimeError('本地服务拒绝了当前凭据。请确认打开的是正在运行的蜂信目录。') from error
        raise RuntimeError(f'本地服务返回 HTTP {error.code}，请检查网关端口和日志。') from error
    try:
        value = json.loads(raw) if raw else None
    except ValueError as error:
        raise RuntimeError('本地端口返回了无法识别的内容，请检查正在运行的程序。') from error
    if path == '/api/status' and (not isinstance(value, dict) or value.get('version') != '1.0.0-fengxin'
                                  or not isinstance(value.get('modem'), dict)):
        raise RuntimeError('7576 端口运行的服务与当前蜂信不匹配。')
    return value


def connection_file(cfg):
    write_private(BASE / '手机SIP配置.txt', f'''蜂信 Mac 手机 SIP 连接参数（仅保存在本机，请勿公开）

手机和 Mac 必须连接同一 Wi-Fi；SIP 客户端选择“已有 SIP 账户 / 第三方 SIP 账户”。
服务器 / 域名：{cfg['sip_advertise_ip']}:5068
服务器端口：5068
用户名：{cfg['sip_username']}
认证 ID：{cfg['sip_username']}
密码：{cfg['sip_password']}
传输：UDP
认证域（Realm，仅客户端要求时填写）：vohive.local
音频编码：PCMU / G.711 μ-law / u-law，8000 Hz
STUN / ICE：关闭
媒体加密：关闭（此版为局域网 RTP）

注册成功后，在手机 SIP 客户端输入你要呼叫的号码，确认后拨打。
接听与说话都使用手机。手机客户端保持在线；本版未接入后台推送。

Mac 管理网页：http://{cfg['web_listen']}
网页用户名：admin
网页密码：{cfg['web_password']}

网页可发送短信、查看收件箱和模组状态。Mac 网关运行时请勿同时使用 EC20-Mac 控制台。
如 Mac 的 Wi-Fi 地址改变，先停止再启动网关，并重新读取此文件更新手机服务器地址。
''')


def main():
    (BASE / 'data').mkdir(exist_ok=True, mode=0o700)
    (BASE / 'logs').mkdir(exist_ok=True, mode=0o700)
    (BASE.parent / 'EC20-Mac' / 'data').mkdir(parents=True, exist_ok=True, mode=0o700)
    with (BASE / 'data' / 'launcher.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        cfg = json.loads(CONFIG.read_text()) if CONFIG.exists() else None
        operation = sys.argv[1] if len(sys.argv) > 1 else 'start'
        if operation == 'stop':
            if not cfg:
                print('尚未配置网关。'); return
            recovery = managed_active(BASE)
            session = read_session(BASE)
            if recovery and session:
                # This supervisor stops its own child when it observes the
                # session change, even between USB recovery attempts.
                session['desired_running'] = False
                private_write(BASE / 'data' / 'recovery-session.json', json.dumps(session) + '\n')
            else:
                try:
                    request(cfg)  # Verify the service before asking it to exit.
                    request(cfg, '/api/shutdown', {})
                except (urllib.error.URLError, TimeoutError, ConnectionError) as exc:
                    raise RuntimeError('未能联系网关，未执行强制终止。') from exc
            for _ in range(60):
                try:
                    request(cfg)
                except (urllib.error.URLError, TimeoutError, ConnectionError):
                    shared = BASE.parent / 'EC20-Mac' / 'data' / 'modem.lock'
                    with shared.open('a') as device_lock:
                        try:
                            fcntl.flock(device_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                            if not managed_active(BASE):
                                print('网关已停止，USB 接口已释放。'); return
                        except BlockingIOError:
                            pass
                time.sleep(0.25)
            raise RuntimeError('已请求停止，但服务仍可访问，请检查日志。')
        if operation not in ('start', 'status'):
            raise ValueError('Expected start, stop or status')
        if cfg:
            try:
                status = request(cfg)
                if '--desktop' in sys.argv and not status.get('desktop'):
                    raise RuntimeError('请先停止手机 SIP 网关，再启动电脑通话模式。')
                if not cfg.get('desktop'): connection_file(cfg)
                print(json.dumps({'running': True, 'version': status['version'], 'modem': status['modem'],
                                  'sip_listen': status['sip_listen'], 'sip_registered': status['sip_registered']}, ensure_ascii=False))
                if operation == 'start' and '--no-open' not in sys.argv and not cfg.get('desktop'):
                    subprocess.run(['/usr/bin/open', str(BASE / '手机SIP配置.txt')])
                    subprocess.run(['/usr/bin/open', 'http://' + cfg['web_listen']])
                return
            except (urllib.error.URLError, TimeoutError, ConnectionError) as error:
                if not isinstance(getattr(error, 'reason', error), ConnectionRefusedError):
                    raise RuntimeError('本地端口暂时无法确认，请稍后重试。') from error
                if operation == 'status':
                    raise RuntimeError('网关未运行。')
        if operation == 'status':
            raise RuntimeError('网关尚未配置。')
        if cfg is None:
            # Another installation might be listening. Probe without sending
            # credentials or writing a configuration on the first launch.
            try:
                with socket.create_connection(('127.0.0.1', 7576), timeout=3):
                    pass
            except ConnectionRefusedError:
                pass
            except OSError as error:
                raise RuntimeError('本地端口暂时无法确认，请稍后重试。') from error
            else:
                raise RuntimeError('本地端口已被另一个服务占用。请先退出占用端口的程序。')
        if managed_active(BASE):
            # A recovery already owns this session. Never spawn another gateway
            # merely because HTTP is briefly unavailable during reconnect.
            if not cfg:
                raise RuntimeError('现有服务仍在运行，但缺少本地配置，无法确认身份。')
            for _ in range(45):
                if not managed_active(BASE):
                    raise RuntimeError('原服务已停止，请重新连接设备。')
                try:
                    status = request(cfg)
                    if '--desktop' in sys.argv and not status.get('desktop'):
                        raise RuntimeError('请先停止手机SIP网关，再启动电脑通话模式。')
                    print(json.dumps({'running': True, 'modem': status['modem']}, ensure_ascii=False))
                    return
                except (urllib.error.URLError, TimeoutError, ConnectionError):
                    time.sleep(1)
            raise RuntimeError('设备正在恢复连接，请查看日志；请勿重复启动。')
        desktop = '--desktop' in sys.argv or bool(cfg and cfg.get('desktop'))
        ip = '127.0.0.1' if desktop else lan_ip()
        if cfg is None:
            cfg = {'sip_username':'1001', 'sip_password':secrets.token_urlsafe(18),
                   'web_password':secrets.token_urlsafe(15)}
        cfg.update(desktop=desktop, at_port='usb:2c7c:0125', audio_device='AC Interface|AS Interface',
                   sip_listen=ip + ':5068', sip_advertise_ip=ip, web_listen='127.0.0.1:7576')
        write_private(CONFIG, json.dumps(cfg, ensure_ascii=False, indent=2) + '\n')
        write_private(BASE / 'runtime.json', json.dumps({'python':sys.executable}) + '\n')
        if not desktop: connection_file(cfg)
        with (BASE / 'logs' / 'launcher.log').open('ab') as log:
            if desktop:
                session = secrets.token_hex(16)
                private_write(BASE / 'data' / 'recovery-session.json', json.dumps({'session': session, 'desired_running': True}) + '\n')
                command = [sys.executable, str(BASE / 'device_supervisor.py'), session]
            else:
                command = [str(BASE / 'fengxin-core'), '-config', str(CONFIG)]
            child = subprocess.Popen(command, cwd=BASE,
                                     stdin=subprocess.DEVNULL, stdout=log, stderr=log, start_new_session=True)
        if not desktop:
            write_private(BASE / 'data' / 'gateway.pid', str(child.pid) + '\n')
        for _ in range(45):
            if child.poll() is not None:
                raise RuntimeError('启动失败，请查看 logs/launcher.log。')
            try:
                status = request(cfg)
                print(json.dumps({'running':True, 'pid':child.pid, 'web':'http://' + cfg['web_listen'],
                                  'sip_listen':cfg['sip_listen'], 'modem':status['modem']}, ensure_ascii=False))
                if '--no-open' not in sys.argv and not cfg.get('desktop'):
                    subprocess.run(['/usr/bin/open', str(BASE / '手机SIP配置.txt')])
                    subprocess.run(['/usr/bin/open', 'http://' + cfg['web_listen']])
                return
            except (urllib.error.URLError, TimeoutError, ConnectionError):
                time.sleep(1)
        raise RuntimeError('启动仍未就绪，请查看 logs/launcher.log；请勿重复启动。')


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        print(str(exc), file=sys.stderr)
        sys.exit(1)
