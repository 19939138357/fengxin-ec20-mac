import SwiftUI
import AppKit
import Combine
import AVFoundation

private let delivery = Bundle.main.bundleURL.deletingLastPathComponent()
private let defaultMailbox = ""

struct SMS: Identifiable, Decodable {
    let id: String
    let number: String
    let text: String
    let receivedAt: String
    let historical: Bool
}
struct MailEvent: Identifiable, Decodable {
    let id: String
    let title: String
    let status: String
    let occurredAt: String
    let lastError: String?
}
struct AudioDevice: Decodable, Hashable { let name: String; let sampleRate: Double }

enum Page: String, CaseIterable, Identifiable {
    case home = "总览", phone = "电话", sms = "短信", mail = "邮箱转发", device = "设备与设置"
    var id: String { rawValue }
    var icon: String { switch self { case .home: "square.grid.2x2.fill"; case .phone: "phone.fill"; case .sms: "message.fill"; case .mail: "envelope.fill"; case .device: "hifispeaker.fill" } }
}

@MainActor final class AppModel: ObservableObject {
    @Published var page: Page = .home
    @Published var starting = true
    @Published var online = false
    @Published var registered = false
    @Published var simReady = false
    @Published var registrationLabel = "尚未注册网络"
    @Published var radioDetails = ""
    @Published var banner = ""
    @Published var number = ""
    @Published var smsNumber = ""
    @Published var smsText = ""
    @Published var smsResult = ""
    @Published var sendingSMS = false
    @Published var busyCall = false
    @Published var callState = "idle"
    @Published var callNumber = ""
    @Published var callError = ""
    @Published var incoming = false
    @Published var muted = false
    @Published var connectedAt: Date?
    @Published var messages: [SMS] = []
    @Published var receiverText = "正在读取短信归档…"
    @Published var inputs: [AudioDevice] = []
    @Published var outputs: [AudioDevice] = []
    @Published var input = ""
    @Published var output = ""
    @Published var audioSaved = false
    @Published var from = defaultMailbox
    @Published var to = defaultMailbox
    @Published var authorization = ""
    @Published var mailEnabled = false
    @Published var passwordConfigured = false
    @Published var mailLocked = false
    @Published var mailResult = ""
    @Published var savingMail = false
    @Published var events: [MailEvent] = []
    private let directory: URL
    private let localSession: URLSession
    private static func makeSession() -> URLSession {
        let cfg=URLSessionConfiguration.ephemeral
        cfg.connectionProxyDictionary=[:]
        return URLSession(configuration:cfg)
    }
    @Published var serviceReady = false
    @Published var signalText = "暂无读数"
    @Published var callStatusKnown = false
    @Published var mailStatusKnown = false
    private var mailFormLoaded = false
    @Published var eventsError = ""
    @Published var updatedAt: Date?
    @Published var smsSearch = ""
    @Published var unreadOnly = false
    @Published var selectedSMS: String?
    @Published var local = FXLocal()
    @Published var contactName = ""
    @Published var diagnostic = ""
    @Published var diagnosing = false
    @Published var showDetails = false
    @Published var mailSavedEnabled = false
    @Published var storageText = ""
    @Published var dataError = ""
    @Published var fileWaiting=false
    @Published var theme="浅色"
    private var currentCallID: String?
    private var currentCallConnected = false
    private var launched = false
    private var refreshing = false
    private var tickCount = 0
    private var refreshLoop: Task<Void,Never>?
    private var lastRingSound = Date.distantPast
    private let decoder: JSONDecoder = { let d=JSONDecoder();d.keyDecodingStrategy = .convertFromSnakeCase;return d }()

    var callLabel: String { if callState != "idle" && (!online || !callStatusKnown) { return "上次通话状态" }; switch callState {case "ringing": return incoming ? "有来电":"对方正在振铃";case "dialing": return "正在呼叫";case "answering": return "正在接听";case "connected": return "通话中";default:return "拨号"} }
    var audioReady: Bool { !input.isEmpty && !output.isEmpty && inputs.contains(where:{$0.name==input}) && outputs.contains(where:{$0.name==output}) && audioSaved }
    var statusTitle: String {
        if fileWaiting { return "等待文件访问" }
        if starting { return "正在连接本地服务" }
        if !serviceReady { return "本地服务未连接" }
        if !online { return "未连接 EC20" }
        if !simReady { return "SIM 未就绪" }
        return registered ? "网络已注册" : registrationLabel
    }
    var cellularBlock: String? {
        if starting { return "正在连接本地服务…" }
        if !serviceReady { return "请在「设备与设置」中启动本地服务。" }
        if !online { return "请通过 USB 连接 EC20，蜂信会自动检测。" }
        if !simReady { return "等待 EC20 读取 SIM 卡。" }
        if !registered { return registrationLabel + "，请等网络注册成功。" }
        return nil
    }
    var phoneBlock: String {
        if !dataLoaded { return "等本地设置读取完成后即可拨号。" }
        if let reason = cellularBlock { return reason }
        if !callStatusKnown { return "等待读取通话状态。" }
        if !audioReady { return "请先选择并保存麦克风与扬声器。" }
        if !FXRules.validNumber(number) { return "请输入有效的电话号码。" }
        return "通过 EC20 内的 SIM 卡呼叫。"
    }
    var canDial: Bool {
        dataLoaded && cellularBlock == nil && callStatusKnown && audioReady &&
            FXRules.validNumber(number) && !busyCall && callState == "idle"
    }
    var canSend: Bool {
        dataLoaded && cellularBlock == nil && !sendingSMS &&
            FXRules.validNumber(smsNumber) && FXRules.validSMS(smsText)
    }
    var canRestart: Bool {
        !starting && !busyCall && !sendingSMS && !diagnosing &&
            (!serviceReady || !online || (callStatusKnown && callState == "idle"))
    }
    var canDiagnose: Bool {
        !starting && online && callStatusKnown && callState == "idle" &&
            !busyCall && !sendingSMS && !diagnosing
    }
    var unread: Int { messages.filter{!$0.historical && !local.readIDs.contains($0.id)}.count }
    var filteredMessages:[SMS] {messages.filter { m in
        (!unreadOnly || (!m.historical && !local.readIDs.contains(m.id))) && (smsSearch.isEmpty || m.number.localizedCaseInsensitiveContains(smsSearch) || m.text.localizedCaseInsensitiveContains(smsSearch))
    }}
    var selectedMessage:SMS? { messages.first{$0.id==selectedSMS} }
    var mailState:String { if !serviceReady{return "等待连接"};if !mailStatusKnown{return "待确认"};return mailLocked ? "等待授权" : (!passwordConfigured ? "待配置" : (mailSavedEnabled ? "已启用":"已暂停")) }
    private let storeQueue=DispatchQueue(label:"com.zjx.fengxin.local-store")
    @Published var dataLoaded=false
    init(directory: URL = delivery, session: URLSession? = nil) {
        self.directory = directory
        self.localSession = session ?? Self.makeSession()
    }
    func loadLocal() async {
        let files=await Task.detached{ [directory] () -> (Data?,Data?,Bool) in
            var ui:Data?;var accessible=true
            do{ui=try Data(contentsOf:directory.appendingPathComponent("data/fengxin-ui.json"))}catch{let e=error as NSError;accessible=e.domain==NSCocoaErrorDomain && e.code==NSFileReadNoSuchFileError}
            let sms=try? Data(contentsOf:directory.appendingPathComponent("data/sms/received.json"))
            return (ui,sms,accessible)
        }.value
        var valid=files.2
        if let data=files.0 {
            if let saved=try? JSONDecoder().decode(FXLocal.self,from:data){local=saved;theme=saved.theme;if number.isEmpty{number=saved.number};if smsNumber.isEmpty{smsNumber=saved.smsNumber};if smsText.isEmpty{smsText=saved.draft}}else{valid=false}
        }
        if let data=files.1,let state=try? JSONSerialization.jsonObject(with:data) as? [String:Any],let map=state["messages"] as? [String:Any],let bytes=try? JSONSerialization.data(withJSONObject:Array(map.values)),let saved=try? decoder.decode([SMS].self,from:bytes){messages=saved.sorted{$0.receivedAt>$1.receivedAt};receiverText="\(messages.count) 条本地记录 · 连接设备后接收新短信"}
        dataLoaded=valid;dataError=valid ? "":"本地设置未能读取，原文件已保留。请检查文件访问权限；此时不会覆盖原联系人和草稿。"
    }
    func persist() {
        guard dataLoaded else{if dataError.isEmpty{dataError="正在等待读取设置，草稿暂未保存。"};return}
        local.number=number;local.smsNumber=smsNumber;local.draft=smsText;local.theme=theme
        local.records=Array(local.records.prefix(200));local.readIDs=Array(local.readIDs.suffix(2000))
        guard let bytes=try? JSONEncoder().encode(local) else{return}
        storeQueue.async { [weak self, directory] in
            do {let d=directory.appendingPathComponent("data");try FileManager.default.createDirectory(at:d,withIntermediateDirectories:true,attributes:[.posixPermissions:0o700]);let url=d.appendingPathComponent("fengxin-ui.json");try bytes.write(to:url,options:.atomic);try FileManager.default.setAttributes([.posixPermissions:0o600],ofItemAtPath:url.path)}catch{let text=error.localizedDescription;Task{@MainActor in self?.dataError="本地草稿未保存："+text}}
        }
    }
    func finishSavingDraft() {
        // A final edit can still be queued when the user quits the menu bar app.
        storeQueue.sync {}
    }
    func markRead(_ id:String?) { selectedSMS=id;if let id,!local.readIDs.contains(id){local.readIDs.append(id);persist()} }
    func reply(_ message:SMS) {smsNumber=message.number;persist()}
    func dialNumber(_ value:String) {number=value;page = .phone;persist()}
    func compose(_ value:String="") {if !value.isEmpty{smsNumber=value};page = .sms;persist()}
    func addContact() {
        guard dataLoaded else{banner="请等待本地设置读取完成。";return};guard FXRules.validNumber(number) else {banner="请先输入有效号码。";return}
        let n=FXRules.number(number);guard !local.contacts.contains(where:{$0.number==n}) else{banner="该号码已在常用联系人中。";return}
        local.contacts.append(FXContact(name:contactName.isEmpty ? n:contactName,number:n));contactName="";persist();banner="已加入常用联系人。"
    }
    func api(_ path: String, body: [String:Any]? = nil) async throws -> Data {
        let raw=try await Task.detached{[directory] in try Data(contentsOf:directory.appendingPathComponent("config.json"))}.value
        guard let cfg=try JSONSerialization.jsonObject(with:raw) as? [String:Any], let password=cfg["web_password"] as? String,let address=cfg["web_listen"] as? String, address=="127.0.0.1:7576" else {throw AppError("无法读取本机连接配置")}
        let origin="http://"+address
        var request=URLRequest(url:URL(string:origin+path)!,timeoutInterval:(path=="/api/sms" || path=="/api/notifications/unlock" || (path=="/api/notifications/settings" && body != nil)) ? 75:8)
        request.setValue("Basic "+Data(("admin:"+password).utf8).base64EncodedString(),forHTTPHeaderField:"Authorization")
        if let body {request.httpMethod="POST";request.setValue(origin,forHTTPHeaderField:"Origin");request.setValue("application/json",forHTTPHeaderField:"Content-Type");request.httpBody=try JSONSerialization.data(withJSONObject:body)}
        let (data,response)=try await localSession.data(for:request)
        guard let response=response as? HTTPURLResponse,(200..<300).contains(response.statusCode) else {throw AppError(String(data:data,encoding:.utf8)?.trimmingCharacters(in:.whitespacesAndNewlines) ?? "服务没有返回有效结果")}
        return data
    }
    func dictionary(_ path: String, body: [String:Any]? = nil) async throws -> [String:Any] {let data=try await api(path,body:body);return (try JSONSerialization.jsonObject(with:data)) as? [String:Any] ?? [:]}
    nonisolated static func launcher(_ operation: String) -> String? {
        let process=Process();process.executableURL=delivery.appendingPathComponent("runtime/bin/python3")
        process.arguments=[delivery.appendingPathComponent("launcher.py").path,operation,"--desktop","--no-open"]
        process.currentDirectoryURL=delivery;process.standardOutput=FileHandle.nullDevice
        let error=Pipe();process.standardError=error
        do {
            try process.run()
            let deadline=Date().addingTimeInterval(operation=="stop" ? 12:55)
            while process.isRunning && Date()<deadline {Thread.sleep(forTimeInterval:0.1)}
            if process.isRunning {process.terminate();return "连接超时。请检查设备连接后重试。"}
            let bytes=error.fileHandleForReading.readDataToEndOfFile()
            return process.terminationStatus==0 ? nil:String(data:bytes,encoding:.utf8) ?? "连接失败"
        }catch{return error.localizedDescription}
    }
    func start(force:Bool=false) async {
        guard !launched || force else{return};launched=true;starting=true;banner=""
        if !dataLoaded {
            let watchdog=Task {try? await Task.sleep(nanoseconds:15_000_000_000);if !Task.isCancelled && !dataLoaded{fileWaiting=true;banner="正在等待本地文件访问。若 macOS 提示访问文稿，请允许蜂信读取本项目中的短信和设置。"}}
            await loadLocal();watchdog.cancel();fileWaiting=false;banner=""
        }
        let error=await Task.detached { AppModel.launcher("start") }.value
        starting=false
        if let error {banner=error}
        await refresh();if serviceReady{await loadAudio();await loadMail()}
        if refreshLoop == nil { refreshLoop = Task { [weak self] in
            while !Task.isCancelled {try? await Task.sleep(nanoseconds:2_000_000_000);guard let self else{return};if !self.starting{await self.refresh()}}
        }}
    }
    func refresh(all: Bool = false) async {
        guard !refreshing else { return }
        refreshing = true
        defer { refreshing = false }
        do {
            let status = try await dictionary("/api/status")
            guard status["desktop"] as? Bool == true, let modem = status["modem"] as? [String:Any],
                  let connected = modem["connected"] as? Bool else {
                throw AppError("本地服务未返回桌面设备状态。")
            }
            serviceReady = true
            online = connected
            simReady = connected && (modem["sim"] as? String ?? "").components(separatedBy: .newlines)
                .contains { $0.trimmingCharacters(in: .whitespaces) == "+CPIN: READY" }
            registered = simReady && modem["registered"] as? Bool == true
            registrationLabel = FXRules.registrationLabel(modem["registration"] as? String ?? "")
            signalText = online ? FXRules.signal(modem["signal"] as? String ?? "") : "暂无读数"
            updatedAt = online ? (modem["last_updated"] as? String).flatMap(FXRules.date) : nil
            let details = online ? ["model","sim","registration","signal","operator","error"] : ["error"]
            radioDetails = details.compactMap { modem[$0] as? String }.filter { !$0.isEmpty }.joined(separator: "\n\n")
            if let error = status["notification_error"] as? String, !error.isEmpty { mailResult = error }
        } catch {
            serviceReady = false
            online = false
            simReady = false
            registered = false
            callStatusKnown = false
            mailStatusKnown = false
            updatedAt = nil
            radioDetails = ""
            signalText = "暂无读数"
            receiverText = "本地服务未连接，显示已读取的短信。"
            storageText = ""
            NSApp?.dockTile.badgeLabel = nil
            return
        }

        do {
            let call = try await dictionary("/api/desktop/call")
            guard call["state"] is String else { throw AppError("通话状态暂不可用。") }
            callStatusKnown = true
            updateCall(call)
        } catch {
            callStatusKnown = false
        }
        if online && callStatusKnown && callState == "ringing" && incoming {
            NSApp?.dockTile.badgeLabel = "来电"
            if Date().timeIntervalSince(lastRingSound) > 8 {
                NSSound(named: "Glass")?.play()
                lastRingSound = Date()
            }
        } else {
            NSApp?.dockTile.badgeLabel = nil
            lastRingSound = .distantPast
        }

        tickCount += 1
        guard all || tickCount % 3 == 1 else { return }
        do {
            let inbox = try await dictionary("/api/inbox")
            guard let list = inbox["messages"] as? [[String:Any]] else { throw AppError("收件箱格式无效") }
            messages = try decoder.decode([SMS].self, from: JSONSerialization.data(withJSONObject: list))
            let receiver = inbox["receiver"] as? [String:Any] ?? [:]
            if !online {
                receiverText = "本地记录 · 连接 EC20 后接收新短信"
            } else if let error = receiver["error"] as? String, !error.isEmpty {
                receiverText = error
            } else {
                receiverText = receiver["ready"] as? Bool == true ? "正在接收新短信 · \(messages.count) 条记录" : "收件箱正在准备"
            }
            storageText = online && receiver["storage_full"] as? Bool == true ? "模组短信存储已满，请先备份记录后清理。" : ""
        } catch {
            receiverText = "收件箱刷新失败。已读取的短信仍可查看。"
        }
        do {
            events = try decoder.decode([MailEvent].self, from: await api("/api/notifications/events"))
            eventsError = ""
        } catch {
            eventsError = "转发记录暂时无法刷新。"
        }
        do {
            let mail = try await dictionary("/api/notifications/settings")
            applyMailStatus(mail)
        } catch {
            mailStatusKnown = false
        }
    }

    private func applyMailStatus(_ settings: [String:Any]) {
        passwordConfigured = settings["password_configured"] as? Bool ?? false
        mailLocked = settings["password_locked"] as? Bool ?? false
        mailSavedEnabled = settings["enabled"] as? Bool ?? false
        mailStatusKnown = true
        if !mailFormLoaded {
            mailEnabled = mailSavedEnabled
            if let sender = settings["from"] as? String, !sender.isEmpty { from = sender }
            if let recipient = settings["to"] as? String, !recipient.isEmpty { to = recipient }
            mailFormLoaded = true
        }
    }
    private func updateCall(_ s:[String:Any]) {
        let next=s["state"] as? String ?? "idle"
        callNumber=s["number"] as? String ?? "";callError=s["error"] as? String ?? "";incoming=s["incoming"] as? Bool ?? false;muted=s["muted"] as? Bool ?? false
        if next != "idle" && currentCallID == nil {
            let record=FXRecord(number:callNumber,kind:incoming ? "来电":"拨出",result:"进行中")
            currentCallID=record.id;currentCallConnected=false;local.records.insert(record,at:0);persist()
        }
        if next=="connected" {currentCallConnected=true}
        if let id=currentCallID,let index=local.records.firstIndex(where:{$0.id==id}) {
            if !callNumber.isEmpty{local.records[index].number=callNumber}
            local.records[index].result=next=="idle" ? (currentCallConnected ? "通话已结束":"未接通") : (next=="connected" ? "通话中":"呼叫中")
            if next=="idle"{currentCallID=nil;persist()}
        }
        callState=next
        if let value=s["connected_at"] as? String,let date=FXRules.date(value),date.timeIntervalSince1970>0{connectedAt=date}else{connectedAt=nil}
    }
    func call(_ action:String) async {
        guard !busyCall, serviceReady, online, callStatusKnown, !starting else{return};if action=="dial" && !canDial{banner=phoneBlock;return};busyCall=true;defer{busyCall=false};banner=""
        let requestedNumber = FXRules.number(number)
        if action=="dial" {number=requestedNumber;persist()}
        if action=="dial" || action=="answer" {
            guard audioReady else{banner="请连接麦克风，并在设备页保存音频选择。";return}
            let allowed=await AVCaptureDevice.requestAccess(for:.audio)
            guard allowed else{banner="麦克风权限未开启，请在系统设置 → 隐私与安全性 → 麦克风中允许 蜂信。";return}
        }
        // Permission prompts may outlast the device or the incoming call.
        guard serviceReady, online, callStatusKnown, !starting else { return }
        if action == "dial" && (cellularBlock != nil || !audioReady || callState != "idle") { return }
        if action == "answer" && (!incoming || callState != "ringing") { return }
        do{updateCall(try await dictionary("/api/desktop/call",body:["action":action,"number":requestedNumber,"muted":!muted]))}catch{banner=error.localizedDescription}
    }
    func sendSMS() async {
        guard canSend else {smsResult="请检查网络、接收号码和短信长度。";return}
        sendingSMS=true;defer{sendingSMS=false};persist();smsResult="正在提交…"
        let n=FXRules.number(smsNumber),text=smsText
        let record=FXRecord(number:n,kind:"短信",result:"提交中",text:text)
        local.records.insert(record,at:0);persist()
        do {
            _ = try await api("/api/sms",body:["number":n,"text":text])
            if let i=local.records.firstIndex(where:{$0.id==record.id}){local.records[i].result="模组已受理"}
            smsResult="已提交给模组。请以接收手机实际收到为准。"
            if FXRules.number(smsNumber) == n && smsText == text { smsText = "" }
            persist()
        }catch{
            if let i=local.records.firstIndex(where:{$0.id==record.id}){local.records[i].result="未确认成功"}
            smsResult="结果未确认，草稿已保留。请先核实收件，避免重复发送。";persist()
        }
    }
    func loadAudio() async {
        do {let s=try await dictionary("/api/desktop/audio"),d=s["devices"] as? [String:Any] ?? [:],chosen=s["selected"] as? [String:Any] ?? [:]
            inputs=try decoder.decode([AudioDevice].self,from:JSONSerialization.data(withJSONObject:d["inputs"] ?? []));outputs=try decoder.decode([AudioDevice].self,from:JSONSerialization.data(withJSONObject:d["outputs"] ?? []))
            input=chosen["input"] as? String ?? "";output=chosen["output"] as? String ?? "";audioSaved = !input.isEmpty && !output.isEmpty
            if input.isEmpty && inputs.count==1 {input=inputs[0].name};if output.isEmpty {output=outputs.first?.name ?? ""}
        }catch{banner=error.localizedDescription}
    }
    func saveAudio() async {do{_ = try await api("/api/desktop/audio",body:["input":input,"output":output]);audioSaved=true;banner="音频设备已保存。"}catch{banner=error.localizedDescription}}
    func loadMail() async {
        do {let s=try await dictionary("/api/notifications/settings");applyMailStatus(s);mailEnabled=mailSavedEnabled
            if let f=s["from"] as? String,!f.isEmpty{from=f};if let t=s["to"] as? String,!t.isEmpty{to=t}
            if mailLocked{mailResult=s["credential_error"] as? String ?? "请解锁邮箱授权码。"}
        }catch{mailResult=error.localizedDescription}
    }
    func unlockMail() async {
        guard !savingMail else{return};savingMail=true;defer{savingMail=false};mailResult="请在 macOS 提示中允许访问原来的邮箱密钥。"
        do{_ = try await api("/api/notifications/unlock",body:[:]);await loadMail();mailResult="邮箱授权码已解锁。"}catch{mailResult=error.localizedDescription}
    }
    func saveMail() async {
        guard !savingMail else{return};guard FXRules.mail(from) && FXRules.mail(to) else{mailResult="请填写有效的发件与接收邮箱。";return};savingMail=true;defer{savingMail=false}
        do {let s=try await dictionary("/api/notifications/settings",body:["enabled":mailEnabled,"sms_only":true,"host":"smtp.qq.com","port":465,"security":"tls","username":from.trimmingCharacters(in:.whitespacesAndNewlines),"from":from.trimmingCharacters(in:.whitespacesAndNewlines),"to":to.trimmingCharacters(in:.whitespacesAndNewlines),"password":authorization.trimmingCharacters(in:.whitespacesAndNewlines)])
            passwordConfigured=s["password_configured"] as? Bool ?? false;mailSavedEnabled=s["enabled"] as? Bool ?? false;mailLocked=s["password_locked"] as? Bool ?? false;authorization="";mailResult=mailEnabled ? "已保存并启用。之后收到的新短信将自动转发。":"已保存，自动转发已关闭。"
        }catch{mailResult=error.localizedDescription}
    }
    func testMail() async {guard !savingMail else{return};savingMail=true;defer{savingMail=false};do{_ = try await api("/api/notifications/test",body:[:]);mailResult="测试邮件已进入发送队列，请查看下方结果并确认 QQ 邮箱收件。"}catch{mailResult=error.localizedDescription}}
    func restart() async {
        guard canRestart else{banner="请等待当前电话、短信或诊断结束。";return}
        starting=true
        if serviceReady {let error=await Task.detached{AppModel.launcher("stop")}.value;if let error {starting=false;banner=error;return}}
        await start(force:true)
    }
    func diagnose() async {
        guard canDiagnose else{return};diagnosing=true;defer{diagnosing=false};diagnostic="正在读取设备状态…"
        do {
            _ = try await dictionary("/api/network",body:["action":"radio-diagnose"])
            for _ in 0..<35 {try await Task.sleep(nanoseconds:1_000_000_000);let j=try await dictionary("/api/network");if j["state"] as? String != "running"{let r=j["result"] as? [String:Any] ?? [:];diagnostic=((r["log"] as? [String]) ?? []).joined(separator:"\n");if let e=j["error"] as? String{diagnostic+="\n"+e};return}}
            diagnostic="诊断仍在后台进行，请稍后刷新。"
        }catch{diagnostic="诊断未完成："+error.localizedDescription}
    }
}
struct AppError: LocalizedError {let message:String;init(_ message:String){self.message=message};var errorDescription:String?{message}}


private let accent=Color(red:0.18,green:0.43,blue:0.87)
struct Card<Content:View>:View {
    let title:String;let icon:String;@ViewBuilder var content:Content
    init(_ title:String,icon:String="",@ViewBuilder content:()->Content){self.title=title;self.icon=icon;self.content=content()}
    var body:some View{VStack(alignment:.leading,spacing:16){HStack(spacing:8){if !icon.isEmpty{Image(systemName:icon).foregroundStyle(accent)};Text(title).font(.headline)};content}.padding(22).frame(maxWidth:.infinity,alignment:.leading).background(Color(nsColor:.controlBackgroundColor),in:RoundedRectangle(cornerRadius:16)).overlay(RoundedRectangle(cornerRadius:16).stroke(Color.primary.opacity(0.07)))}
}
struct Pill:View {
    var text:String;var color:Color = .secondary
    var body:some View{Text(text).font(.system(size:11,weight:.medium)).foregroundStyle(color).padding(.horizontal,9).padding(.vertical,5).background(color.opacity(0.10),in:Capsule())}
}
struct Empty:View {
    let icon:String,title:String,detail:String
    var body:some View{VStack(spacing:10){Image(systemName:icon).font(.system(size:26)).foregroundStyle(.tertiary);Text(title).font(.subheadline.weight(.medium));Text(detail).font(.caption).foregroundStyle(.secondary).multilineTextAlignment(.center)}.frame(maxWidth:.infinity).padding(.vertical,25)}
}
struct RootView:View {
    @ObservedObject var model:AppModel
    var body:some View {
        NavigationSplitView {
            VStack(alignment:.leading,spacing:8){
                HStack(spacing:12){Image(systemName:"antenna.radiowaves.left.and.right").font(.title2).foregroundStyle(accent).frame(width:42,height:42).background(accent.opacity(0.12),in:RoundedRectangle(cornerRadius:12));VStack(alignment:.leading,spacing:3){Text("蜂信").font(.title2.weight(.semibold));Text("你的桌面通信中心").font(.caption2).foregroundStyle(.secondary)}}.padding(.horizontal,18).padding(.top,22).padding(.bottom,22)
                List(Page.allCases,selection:$model.page){p in HStack{Label(p.rawValue,systemImage:p.icon);Spacer();if p == .sms && model.unread>0{Pill(text:"\(model.unread)",color:accent)}}.tag(p)}.listStyle(.sidebar)
                VStack(alignment:.leading,spacing:10){HStack{Circle().fill(model.registered ? .green:(model.online ? .orange:.secondary)).frame(width:7,height:7);Text(model.statusTitle).font(.caption)};Text("关闭窗口，保持后台运行").font(.caption2).foregroundStyle(.secondary);Text("蜂信 1.0 · Mac").font(.caption2).foregroundStyle(.tertiary)}.padding(18)
            }.navigationSplitViewColumnWidth(min:215,ideal:225,max:245)
        } detail: {
            ScrollView {
                VStack(alignment:.leading,spacing:22){
                    HStack{VStack(alignment:.leading,spacing:7){Text(model.page.rawValue).font(.system(size:29,weight:.semibold));Text(subtitle).font(.callout).foregroundStyle(.secondary)};Spacer();Pill(text:model.statusTitle,color:model.registered ? .green:(model.online ? .orange:.secondary));if model.starting{ProgressView().controlSize(.small)}else{Button{Task{await model.refresh(all: true)}}label:{Image(systemName:"arrow.clockwise")}.help("刷新状态").accessibilityLabel("刷新状态")}}
                    if !model.banner.isEmpty{HStack(alignment:.top,spacing:10){Image(systemName:"info.circle.fill").foregroundStyle(accent);Text(model.banner).font(.callout).textSelection(.enabled);Spacer();Button{model.banner=""}label:{Image(systemName:"xmark")}.buttonStyle(.plain).accessibilityLabel("关闭提示")}.padding(14).frame(maxWidth:.infinity,alignment:.leading).background(accent.opacity(0.08),in:RoundedRectangle(cornerRadius:12))}
                    if !model.dataError.isEmpty{Text(model.dataError).foregroundStyle(.orange)}
                    switch model.page{case .home:home;case .phone:phone;case .sms:sms;case .mail:mail;case .device:device}
                }.padding(28).frame(maxWidth:1100,alignment:.leading).frame(maxWidth:.infinity)
            }.background(Color(nsColor:.windowBackgroundColor))
        }.frame(minWidth:1060,minHeight:740).tint(accent).preferredColorScheme(model.theme=="跟随系统" ? nil:(model.theme=="深色" ? .dark:.light))

    }
    var subtitle:String{switch model.page{case .home:return "电话、短信与自动转发，一处掌握。";case .phone:return "用电脑麦克风与耳机，通过 SIM 卡通话。";case .sms:return "收发短信，快速找到和回复消息。";case .mail:return "新收到的短信，自动送达你的 QQ 邮箱。";case .device:return "连接设备，设置音频，查看真实网络状态。"}}
    var home:some View {
        VStack(spacing:20){
            HStack(alignment:.top,spacing:16){metric("蜂窝网络",value:model.statusTitle,icon:"antenna.radiowaves.left.and.right",note:model.online ? "信号："+model.signalText:"连接 EC20 后读取");metric("短信",value:model.dataLoaded ? "\(model.messages.count) 条":"读取中",icon:"message.fill",note:model.dataLoaded ? "\(model.unread) 条未读 · 本地归档":"等待本地文件访问");metric("邮箱转发",value:model.mailState,icon:"envelope.fill",note:model.to)}
            Card("开始使用",icon:"checklist"){
                step("1","连接 EC20",detail:model.cellularBlock ?? "已注册运营商网络。",ready:model.online){model.page = .device}
                Divider()
                step("2","准备通话音频",detail:model.audioReady ? "麦克风和扬声器已保存":"选择麦克风与扬声器，或连接带麦克风的耳机。",ready:model.audioReady){model.page = .device}
                Divider()
                step("3","启用短信转发",detail:model.mailLocked ? "已保存的授权码需要解锁。":(model.mailSavedEnabled ? "新短信会按已保存的设置转发":"在本机输入 QQ SMTP 授权码，并保存设置。"),ready:model.mailSavedEnabled && !model.mailLocked){model.page = .mail}
            }
            HStack(spacing:14){Button{model.page = .phone}label:{Label("打开拨号盘",systemImage:"phone.fill").frame(maxWidth:.infinity)}.buttonStyle(.borderedProminent);Button{model.compose()}label:{Label("写短信",systemImage:"square.and.pencil").frame(maxWidth:.infinity)}.buttonStyle(.bordered);Button{model.page = .mail}label:{Label("设置自动转发",systemImage:"envelope.badge").frame(maxWidth:.infinity)}.buttonStyle(.bordered)}.controlSize(.large)
            if let message=model.messages.first{Card("最近收到",icon:"message"){HStack{Text(message.number).font(.headline);Spacer();Text(FXRules.time(message.receivedAt)).font(.caption).foregroundStyle(.secondary)};Text(message.text).lineLimit(3);Button("查看短信"){model.page = .sms;model.markRead(message.id)}.buttonStyle(.link)}}
        }
    }
    func metric(_ title:String,value:String,icon:String,note:String)->some View{VStack(alignment:.leading,spacing:14){HStack{Text(title).font(.caption).foregroundStyle(.secondary);Spacer();Image(systemName:icon).foregroundStyle(accent)};Text(value).font(.system(size:21,weight:.semibold));Text(note).font(.caption2).foregroundStyle(.secondary).lineLimit(2)}.padding(20).frame(maxWidth:.infinity,minHeight:125,alignment:.leading).background(Color(nsColor:.controlBackgroundColor),in:RoundedRectangle(cornerRadius:15))}
    func step(_ index:String,_ title:String,detail:String,ready:Bool,action:@escaping()->Void)->some View{HStack(spacing:14){Text(ready ? "✓":index).font(.headline).foregroundStyle(ready ? .green:accent).frame(width:32,height:32).background((ready ? Color.green:accent).opacity(0.1),in:Circle());VStack(alignment:.leading,spacing:5){Text(title).font(.subheadline.weight(.semibold));Text(detail).font(.caption).foregroundStyle(.secondary)};Spacer();Button(index == "1" ? "查看设备" : (ready ? "查看":"设置"),action:action).disabled(model.starting && index=="1")}}
    var phone:some View{
        HStack(alignment:.top,spacing:20){
            VStack(spacing:18){Card(model.callLabel,icon:"phone"){
                if model.callState=="idle"{TextField("输入号码",text:Binding(get:{model.number},set:{model.number=$0;model.persist()})).font(.system(size:30,weight:.medium,design:.rounded)).textFieldStyle(.plain).padding(14).background(Color.primary.opacity(0.04),in:RoundedRectangle(cornerRadius:10)).accessibilityLabel("拨号号码").disabled(!model.dataLoaded)
                    LazyVGrid(columns:Array(repeating:GridItem(.flexible(),spacing:10),count:3),spacing:10){ForEach(["1","2","3","4","5","6","7","8","9","+","0","⌫"],id:\.self){key in Button{if key=="⌫"{if !model.number.isEmpty{model.number.removeLast()}}else{model.number+=key};model.persist()}label:{Text(key).font(.system(size:23,weight:.medium,design:.rounded)).frame(maxWidth:.infinity,minHeight:48)}.buttonStyle(.bordered).accessibilityLabel(key=="⌫" ? "删除最后一位":key)}}.disabled(!model.dataLoaded)
                    Button{Task{await model.call("dial")}}label:{Label("拨打电话",systemImage:"phone.fill").frame(maxWidth:.infinity)}.buttonStyle(.borderedProminent).controlSize(.large).disabled(!model.canDial)
                    Text(model.phoneBlock).font(.caption).foregroundStyle(.secondary)
                }else{if !model.callStatusKnown || !model.online { Text("通话状态待确认").font(.callout) };Text(model.callNumber.isEmpty ? "未知号码":model.callNumber).font(.system(size:30,weight:.medium,design:.rounded));if model.online && model.callStatusKnown && model.callState=="connected",let date=model.connectedAt{Text(date,style:.timer).font(.title2.monospacedDigit())};HStack{if model.callState=="ringing" && model.incoming{Button("接听"){Task{await model.call("answer")}}.buttonStyle(.borderedProminent).disabled(!model.audioReady || model.busyCall || !model.callStatusKnown || !model.online)};Button("挂断"){Task{await model.call("hangup")}}.buttonStyle(.borderedProminent).tint(.red).disabled(model.busyCall || !model.callStatusKnown || !model.online);if model.callState=="connected"{Button(model.muted ? "取消静音":"静音"){Task{await model.call("mute")}}.disabled(model.busyCall || !model.callStatusKnown || !model.online)}}.controlSize(.large)}
                if !model.callError.isEmpty{Text(model.callError).font(.caption).foregroundStyle(.orange)}
            };Card("通话音频",icon:"headphones"){Label(model.input.isEmpty ? "未选择麦克风":model.input,systemImage:"mic");Label(model.output.isEmpty ? "未选择扬声器":model.output,systemImage:"speaker.wave.2");Button("设置音频设备"){model.page = .device}.buttonStyle(.link)}}.frame(maxWidth:.infinity)
            VStack(spacing:18){Card("常用联系人",icon:"person.crop.circle"){
                HStack{TextField("联系人名称（可选）",text:$model.contactName).textFieldStyle(.roundedBorder);Button{model.addContact()}label:{Image(systemName:"plus")}.help("保存当前拨号号码").accessibilityLabel("加入常用联系人").disabled(!model.dataLoaded || !FXRules.validNumber(model.number))}
                if model.local.contacts.isEmpty{Empty(icon:"person.crop.circle.badge.plus",title:model.dataLoaded ? "常用号码，随时找到":"等待读取联系人",detail:model.dataLoaded ? "输入号码后，点加号保存。":"文件访问完成后，加载本地联系人。")}
                ForEach(model.local.contacts){c in HStack{VStack(alignment:.leading,spacing:4){Text(c.name).font(.subheadline.weight(.medium));Text(c.number).font(.caption).foregroundStyle(.secondary)};Spacer();Button{model.number=c.number;model.persist()}label:{Image(systemName:"phone")}.help("填入拨号号码");Button{model.compose(c.number)}label:{Image(systemName:"message")}.help("给此联系人写短信")};Divider()}
            };Card("最近通话",icon:"clock"){
                let records=model.local.records.filter{$0.kind != "短信"}
                if records.isEmpty{Empty(icon:"clock",title:model.dataLoaded ? "暂无通话记录":"等待读取通话记录",detail:"此应用观察到的通话会记录在本机。")}
                ForEach(records.prefix(8)){r in HStack{Image(systemName:r.kind=="来电" ? "phone.arrow.down.left":"phone.arrow.up.right").foregroundStyle(accent);VStack(alignment:.leading,spacing:4){Button(r.number.isEmpty ? "未知号码":r.number){model.number=r.number;model.persist()}.buttonStyle(.plain);Text(r.kind+" · "+FXRules.time(r.time)).font(.caption2).foregroundStyle(.secondary)};Spacer();Pill(text:r.result)};Divider()}
            }}.frame(maxWidth:.infinity)
        }
    }
    var sms:some View {
        VStack(spacing:20){
            Card("写短信",icon:"square.and.pencil"){
                HStack{Text("接收号码").font(.subheadline);TextField("手机号码或服务号码",text:Binding(get:{model.smsNumber},set:{model.smsNumber=$0;model.persist()})).textFieldStyle(.roundedBorder).disabled(!model.dataLoaded)}
                TextEditor(text:Binding(get:{model.smsText},set:{model.smsText=$0;model.persist()})).font(.body).frame(height:100).padding(6).background(Color.primary.opacity(0.025),in:RoundedRectangle(cornerRadius:10)).overlay(RoundedRectangle(cornerRadius:10).stroke(Color.primary.opacity(0.10))).accessibilityLabel("短信正文").disabled(!model.dataLoaded)
                HStack{Text("\(model.smsText.utf16.count)/70 · 单条短信 · "+(model.dataLoaded ? "草稿自动保存":"等待本地数据")).font(.caption).foregroundStyle(model.smsText.utf16.count>70 ? .orange:.secondary);Spacer();if model.sendingSMS{ProgressView().controlSize(.small)};Button{Task{await model.sendSMS()}}label:{Label("发送短信",systemImage:"paperplane.fill")}.buttonStyle(.borderedProminent).disabled(!model.canSend)}
                if let reason = model.cellularBlock { Text(reason).font(.caption).foregroundStyle(.secondary) }
                if !model.smsResult.isEmpty{Text(model.smsResult).font(.callout).textSelection(.enabled)}
            }
            Card("收件箱",icon:"tray"){
                HStack{TextField("搜索号码或短信内容",text:$model.smsSearch).textFieldStyle(.roundedBorder).accessibilityLabel("搜索短信");Toggle("只看未读",isOn:$model.unreadOnly).toggleStyle(.checkbox);Pill(text:model.dataLoaded ? "\(model.filteredMessages.count) 条":"读取中")}
                Text(model.receiverText).font(.caption).foregroundStyle(.secondary)
                if !model.storageText.isEmpty{Text(model.storageText).font(.caption).foregroundStyle(.orange)}
                if model.filteredMessages.isEmpty{Empty(icon:"tray",title:!model.dataLoaded ? "等待读取本地短信":(model.messages.isEmpty ? "暂无短信记录":"没有匹配的短信"),detail:!model.dataLoaded ? "请允许蜂信访问本项目中的短信记录。":(model.messages.isEmpty ? "连接设备后，这里会显示收到的短信。":"试试其他关键词，或关闭未读筛选。"))}
                else{HStack(alignment:.top,spacing:20){List(model.filteredMessages,selection:Binding(get:{model.selectedSMS},set:{model.markRead($0)})){m in VStack(alignment:.leading,spacing:6){HStack{Text(m.number).font(.subheadline.weight(.semibold));if !m.historical && !model.local.readIDs.contains(m.id){Circle().fill(accent).frame(width:6,height:6)}};Text(m.text).font(.caption).lineLimit(2).foregroundStyle(.secondary);Text(FXRules.time(m.receivedAt)).font(.caption2).foregroundStyle(.tertiary)}.padding(.vertical,5).tag(m.id)}.listStyle(.inset).frame(width:260,height:310)
                    VStack(alignment:.leading,spacing:14){if let m=model.selectedMessage{HStack{Text(m.number).font(.headline);Spacer();Pill(text:m.historical ? "历史记录":"收到短信")};Text(FXRules.time(m.receivedAt)).font(.caption).foregroundStyle(.secondary);Text(m.text).font(.body).textSelection(.enabled);Spacer();HStack{Button("回复短信"){model.reply(m)}.buttonStyle(.borderedProminent);Button("填入拨号"){model.dialNumber(m.number)}}}else{Empty(icon:"text.bubble",title:"选择一条短信",detail:"查看完整内容，或快速回复。")}}.frame(maxWidth:.infinity,minHeight:310,alignment:.topLeading)
                }}
            }
            let sent=model.local.records.filter{$0.kind=="短信"}
            if !sent.isEmpty{Card("发送记录",icon:"paperplane"){ForEach(sent.prefix(5)){r in HStack{VStack(alignment:.leading,spacing:5){Text(r.number).font(.subheadline.weight(.medium));Text(r.text).font(.caption).lineLimit(1).foregroundStyle(.secondary);Text(FXRules.time(r.time)).font(.caption2).foregroundStyle(.tertiary)};Spacer();Pill(text:r.result)};Divider()}}}
        }
    }
    var mail:some View {
        VStack(spacing:20){Card("短信自动转发",icon:"envelope.badge"){
            HStack{VStack(alignment:.leading,spacing:6){Text("收到短信 → QQ 邮箱").font(.title3.weight(.semibold));Text("只转发启用之后新收到的短信，包含号码、时间与正文。").font(.callout).foregroundStyle(.secondary)};Spacer();Pill(text:model.mailState,color:model.mailLocked ? .orange:(model.mailSavedEnabled ? .green:.secondary))}
            Divider()
            Grid(alignment:.leading,horizontalSpacing:22,verticalSpacing:16){GridRow{Text("发件 QQ 邮箱");TextField("发件邮箱",text:$model.from).textFieldStyle(.roundedBorder)};GridRow{Text("接收邮箱");TextField("接收邮箱",text:$model.to).textFieldStyle(.roundedBorder)};GridRow{Text("SMTP 授权码");SecureField(model.passwordConfigured ? "已保存；留空保留":"仅在本机输入授权码",text:$model.authorization).textFieldStyle(.roundedBorder)}}
            Text("在 QQ 邮箱的第三方客户端服务中获取 SMTP 授权码。使用授权码，不填写 QQ 登录密码。").font(.caption).foregroundStyle(.secondary)
            Toggle("自动转发新收到的短信",isOn:$model.mailEnabled).toggleStyle(.switch)
            HStack{Button("保存设置"){Task{await model.saveMail()}}.buttonStyle(.borderedProminent).disabled(!model.serviceReady || model.savingMail || !model.mailStatusKnown || !FXRules.mail(model.from) || !FXRules.mail(model.to));if model.mailLocked{Button("解锁邮箱授权码"){Task{await model.unlockMail()}}.disabled(!model.serviceReady || model.savingMail)};Button("发送测试邮件"){Task{await model.testMail()}}.disabled(!model.mailStatusKnown || !model.passwordConfigured || model.mailLocked || !model.mailSavedEnabled || model.savingMail || !model.serviceReady);Link("打开 QQ 邮箱",destination:URL(string:"https://mail.qq.com/")!);if model.savingMail{ProgressView().controlSize(.small)}}
            if !model.mailResult.isEmpty{Text(model.mailResult).font(.callout).textSelection(.enabled)}
            Label(!model.serviceReady ? "连接服务后读取授权码状态":(model.passwordConfigured ? "授权码已加密保存在本机":"尚未配置授权码"),systemImage:"lock.shield").font(.caption).foregroundStyle(.secondary)
        }
        Card("转发结果",icon:"clock.arrow.circlepath"){
            if !model.eventsError.isEmpty { Text(model.eventsError).font(.caption).foregroundStyle(.secondary) }
            else if model.events.isEmpty{Empty(icon:"envelope",title:model.serviceReady ? "暂无转发记录":"等待读取转发记录",detail:model.serviceReady ? "保存设置后，可以先发送一封测试邮件。":"连接本地服务后，读取实际转发结果。")}
            ForEach(model.events.prefix(20)){e in HStack(alignment:.top){VStack(alignment:.leading,spacing:5){Text(e.title).font(.subheadline.weight(.medium));Text(FXRules.time(e.occurredAt)).font(.caption2).foregroundStyle(.secondary);if let error=e.lastError,!error.isEmpty{Text(error).font(.caption).foregroundStyle(.orange).textSelection(.enabled)}};Spacer();Pill(text:mailLabel(e.status),color:e.status=="sent" ? .green:(e.status=="failed" || e.status=="uncertain" ? .orange:.secondary))};Divider()}
            Text("“服务器已接受”需以邮箱实际收到为准。结果不确定时不会自动重发。").font(.caption).foregroundStyle(.secondary)
        }}
    }
    func mailLabel(_ value:String)->String{["sent":"服务器已接受","disabled":"转发已关闭","queued":"等待发送","sending":"发送中","retry":"等待重试","failed":"发送失败","uncertain":"结果未确认","cancelled":"已取消"][value] ?? value}
    var device:some View {
        VStack(spacing:20){
            Card("连接与网络",icon:"antenna.radiowaves.left.and.right"){
                HStack{Text("本地服务").font(.subheadline);Spacer();Pill(text:model.serviceReady ? "已就绪":(model.fileWaiting ? "等待文件访问":(model.starting ? "正在连接":"未连接")),color:model.serviceReady ? .green:.secondary)}
                HStack(spacing:24){VStack(alignment:.leading,spacing:8){Text("设备").font(.caption).foregroundStyle(.secondary);Text(model.online ? "EC20 已连接":(model.serviceReady ? "未连接 EC20":"等待读取")).font(.headline)};Divider().frame(height:42);VStack(alignment:.leading,spacing:8){Text("SIM 卡").font(.caption).foregroundStyle(.secondary);Text(model.online ? (model.simReady ? "已就绪":"未就绪"):"待设备连接").font(.headline)};Divider().frame(height:42);VStack(alignment:.leading,spacing:8){Text("信号").font(.caption).foregroundStyle(.secondary);Text(model.signalText).font(.headline)};Spacer()}
                Text(model.cellularBlock ?? "已注册运营商网络。").font(.callout).foregroundStyle(.secondary)
                if !model.serviceReady && !model.starting {
                    Button("启动本地服务") { Task { await model.restart() } }
                        .buttonStyle(.borderedProminent).disabled(!model.canRestart)
                } else if model.online {
                    HStack {
                        Button("读取网络诊断") { Task { await model.diagnose() } }
                            .disabled(!model.canDiagnose)
                        if model.diagnosing { ProgressView().controlSize(.small) }
                    }
                }
                if let date=model.updatedAt{Text("最近读取："+date.formatted(date:.omitted,time:.standard)).font(.caption2).foregroundStyle(.tertiary)}
                DisclosureGroup("设备详细信息", isExpanded: $model.showDetails) {
                    VStack(alignment: .leading, spacing: 10) {
                        Text(model.radioDetails.isEmpty ? "连接设备后显示读数。" : model.radioDetails)
                            .font(.system(.caption, design: .monospaced)).textSelection(.enabled)
                        if model.serviceReady {
                            Button("重新启动本地服务") { Task { await model.restart() } }
                                .disabled(!model.canRestart)
                        }
                    }.frame(maxWidth: .infinity, alignment: .leading)
                }
                if !model.diagnostic.isEmpty{Text(model.diagnostic).font(.system(.caption,design:.monospaced)).textSelection(.enabled)}
            }
            Card("电脑通话音频",icon:"headphones"){
                Picker("麦克风",selection:Binding(get:{model.input},set:{model.input=$0;model.audioSaved=false})){Text("请选择麦克风").tag("");ForEach(model.inputs,id:\.name){Text($0.name).tag($0.name)}}
                Picker("扬声器 / 耳机",selection:Binding(get:{model.output},set:{model.output=$0;model.audioSaved=false})){Text("请选择扬声器").tag("");ForEach(model.outputs,id:\.name){Text($0.name).tag($0.name)}}
                HStack{Button("刷新音频设备"){Task{await model.loadAudio()}}.disabled(!model.serviceReady);Button("保存音频选择"){Task{await model.saveAudio()}}.buttonStyle(.borderedProminent).disabled(!model.serviceReady || model.input.isEmpty || model.output.isEmpty);Pill(text:model.audioReady ? "音频已就绪":"待设置",color:model.audioReady ? .green:.secondary)}.disabled(model.callState != "idle")
                Text("使用带麦克风的耳机可减少回声。插入新设备后，刷新并保存选择。").font(.caption).foregroundStyle(.secondary)
            }
            Card("蜂信 · 关于与数据",icon:"info.circle"){
                Text("你的 Mac 桌面通信工具 · 1.0.0").font(.subheadline.weight(.semibold))
                Picker("界面外观",selection:Binding(get:{model.theme},set:{model.theme=$0;model.persist()})){ForEach(["浅色","深色","跟随系统"],id:\.self){Text($0).tag($0)}}
                Text("关闭窗口后继续在菜单栏运行。退出应用或 Mac 休眠期间，短信接收与转发暂停。").font(.callout).foregroundStyle(.secondary)
                HStack{Button("打开数据文件夹"){NSWorkspace.shared.open(delivery.appendingPathComponent("data"))};Button("查看连接日志"){NSWorkspace.shared.open(delivery.appendingPathComponent("logs/launcher.log"))};Button("组件来源与许可"){NSWorkspace.shared.open(delivery.appendingPathComponent("NOTICE.md"))}}
            }
        }
    }
}
@MainActor final class AppDelegate:NSObject,NSApplicationDelegate {
    weak var model:AppModel?
    func applicationShouldTerminateAfterLastWindowClosed(_ sender:NSApplication)->Bool{false}
    func applicationShouldTerminate(_ sender:NSApplication)->NSApplication.TerminateReply{
        model?.finishSavingDraft()
        guard model?.serviceReady==true else{return .terminateNow}
        Task{_ = await Task.detached{AppModel.launcher("stop")}.value;NSApp.reply(toApplicationShouldTerminate:true)};return .terminateLater
    }
}
#if !FENGXIN_MODEL_TESTS
@main struct FengxinMacApp:App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) var delegate
    @StateObject private var model=AppModel()
    var body:some Scene {
        WindowGroup("蜂信"){RootView(model:model).task{delegate.model=model;await model.start()}}.defaultSize(width:1180,height:820).commands{CommandGroup(replacing:.newItem){}}
        MenuBarExtra("蜂信",systemImage:"antenna.radiowaves.left.and.right"){
            Text(model.statusTitle);Button("打开蜂信"){NSApp.activate(ignoringOtherApps:true);NSApp.windows.first(where:{$0.canBecomeMain})?.makeKeyAndOrderFront(nil)};Button("写短信"){model.compose();NSApp.activate(ignoringOtherApps:true);NSApp.windows.first(where:{$0.canBecomeMain})?.makeKeyAndOrderFront(nil)};Divider();Button("退出蜂信"){NSApp.terminate(nil)}
        }
    }
}
#endif
