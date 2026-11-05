from pathlib import Path
p=Path('cmd/vohive-sip/main.go')
s=p.read_text().replace('0.2.8-windows-sip','0.3.0-macos-sip').replace('"sync/atomic"','"sync/atomic"\n\t"syscall"').replace('context.Background(), os.Interrupt)', 'context.Background(), os.Interrupt, syscall.SIGTERM)').replace('VoHive SIP Windows %s', 'VoHive SIP macOS %s')
s=s.replace('case "/api/sms":','''case "/api/sip-settings":
            if r.Method != "POST" { http.Error(w, "Method", 405); return }
            _, port, _ := net.SplitHostPort(cfg.SIPListen)
            w.Header().Set("Content-Type", "application/json")
            json.NewEncoder(w).Encode(map[string]string{"host":cfg.SIPAdvertiseIP, "port":port, "username":cfg.SIPUsername, "password":cfg.SIPPassword, "realm":"vohive.local", "transport":"UDP", "codec":"PCMU / G.711 μ-law (8000 Hz)"})
        case "/api/sms":''')
p.write_text(s)
p=Path('cmd/vohive-sip/index.html');s=p.read_text()
s=s.replace('EC20 · WINDOWS · SIP','EC20 · macOS · 手机 SIP')
s=s.replace('<section><h2>电话</h2><pre id="sip"></pre><p>打开交付目录中的 <b>Phone.cmd</b>，在 MicroSIP 输入手机号码拨打。来电会在软电话中响铃。</p><small>电脑麦克风和扬声器在 MicroSIP 设置中选择。SIP 账户密码通过 Credentials.cmd 在本机查看。</small></section>', '''<section><h2>手机 SIP 电话</h2><pre id="sip"></pre><p>手机与 Mac 连接同一个 Wi-Fi，在手机 SIP 客户端中添加 SIP 账户。连接后，在手机输入号码拨打，来电也由手机接听。</p><small>传输选择 UDP，音频编码选择 PCMU / G.711 μ-law。关闭 STUN、ICE 和必需的媒体加密。客户端需保持在线；本版未接入手机后台推送。</small><br><button id="showSIP" class="secondary" type="button">显示手机连接参数</button><pre id="sipSettings" hidden></pre></section>''')
a=s.index('<section class="wide"><h2>邮件转发')
b=s.index('<section class="wide"><h2>来电与转发记录',a)
s=s[:a]+'''<section class="wide"><h2>使用提示</h2><p>网页用于查看网络和收发短信，电话声音由手机 SIP 客户端收听。Mac 和 EC20 需保持连接并运行；网络地址变化后，请重启网关并更新手机中的服务器地址。</p><small>当前 Mac 版保留本地来电记录，暂未提供邮件转发配置。</small></section>'''+s[b:]
s=s.replace('来电与转发记录','来电记录').replace('“服务器已接受”表示 SMTP 服务已受理，并不保证进入收件箱。结果不确定时不会自动重发；可先核对邮箱及垃圾箱。','来电事件保存在本机。手机后台是否能持续接听，取决于 SIP 客户端和系统的后台运行能力。')
s=s.replace("if(s.notification_error){$('mailState').textContent=s.notification_error;$('mailState').className='status'}",'')
s=s.replace('串口已断开 · 请重新插好模组后运行 Start.cmd','USB 已断开 · 请重新插好模组后运行 启动VoHive.command')
a=s.index('function applySettings(');b=s.index("$('sms').addEventListener",a);s=s[:a]+s[b:]
a=s.index("$('mailForm').addEventListener");b=s.index('updateSendButton();loadSettings();refresh()',a)
s=s[:a]+'''$('showSIP').onclick=async()=>{if(!$('sipSettings').hidden){$('sipSettings').textContent='';$('sipSettings').hidden=true;$('showSIP').textContent='显示手机连接参数';return}try{let s=await api('/api/sip-settings',{});$('sipSettings').textContent='服务器 / 域名：'+s.host+':'+s.port+'\\n用户名 / 认证 ID：'+s.username+'\\n密码：'+s.password+'\\n传输：'+s.transport+'\\n编码：'+s.codec+'\\n认证域（若要求填写）：'+s.realm;$('sipSettings').hidden=false;$('showSIP').textContent='隐藏手机连接参数'}catch(e){$('sipSettings').textContent=e.message;$('sipSettings').hidden=false}};
'''+s[b:]
s=s.replace('updateSendButton();loadSettings();refresh()','updateSendButton();refresh()')
p.write_text(s)
