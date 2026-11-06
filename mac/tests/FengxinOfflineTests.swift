// Run from the repository root on macOS:
// xcrun swiftc -swift-version 5 -D FENGXIN_MODEL_TESTS mac/app/Fengxin{Core,Mac}.swift mac/tests/FengxinOfflineTests.swift -o .build/fengxin-offline-tests
// .build/fengxin-offline-tests
import Foundation

private final class FixtureProtocol: URLProtocol {
    @MainActor static var routes: [String: (Int, Any)] = [:]
    @MainActor static var pendingSMS: FixtureProtocol?
    @MainActor static var posts = 0

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        Task { @MainActor in
            let path = self.request.url!.path
            if self.request.httpMethod == "POST" {
                Self.posts += 1
                precondition(path == "/api/sms", "unexpected mutation")
                Self.pendingSMS = self
                return
            }
            guard let (status, body) = Self.routes[path] else { fatalError("unexpected path: " + path) }
            self.finish(status, body)
        }
    }
    override func stopLoading() {}
    func finish(_ status: Int, _ body: Any) {
        let reply = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: nil)!
        client?.urlProtocol(self, didReceive: reply, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: try! JSONSerialization.data(withJSONObject: body))
        client?.urlProtocolDidFinishLoading(self)
    }
}

@main private struct FengxinOfflineTests {
    @MainActor static func main() async throws {
        let fm = FileManager.default
        let directory = URL(fileURLWithPath: fm.currentDirectoryPath)
            .appendingPathComponent(".build/test-tmp/native-" + UUID().uuidString)
        try fm.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? fm.removeItem(at: directory) }
        try Data(#"{"web_password":"fixture","web_listen":"127.0.0.1:7576"}"#.utf8)
            .write(to: directory.appendingPathComponent("config.json"))
        let cfg = URLSessionConfiguration.ephemeral
        cfg.protocolClasses = [FixtureProtocol.self]
        let session = URLSession(configuration: cfg)
        defer { session.invalidateAndCancel() }
        let model = AppModel(directory: directory, session: session)
        model.starting = false
        await model.loadLocal()
        model.number = "123"
        model.smsNumber = "456"
        model.smsText = "离线起草"
        model.persist()
        model.finishSavingDraft()

        FixtureProtocol.routes = [
            "/api/desktop/call": (200, ["state":"idle"]),
            "/api/inbox": (200, ["messages":[["id":"fixture", "number":"100", "text":"归档", "received_at":"2020-01-01T12:00:00Z", "historical":true]], "receiver":["ready":false, "error":"serial port closed"]]),
            "/api/notifications/events": (200, []),
            "/api/notifications/settings": (200, ["enabled":false])
        ]
        func modem(connected: Bool, sim: String = "+CPIN: READY", registration: String = "", registered: Bool = false) {
            FixtureProtocol.routes["/api/status"] = (200, ["desktop":true, "modem":[
                "connected":connected, "registered":registered, "sim":sim, "registration":registration,
                "signal":"+CSQ: 30,0", "last_updated":"2020-01-01T12:00:00Z", "operator":"fixture-operator"
            ]])
        }

        modem(connected: false, registered:true)
        await model.refresh(all: true)
        precondition(model.statusTitle == "未连接 EC20" && !model.canSend && !model.canDial)
        precondition(!model.simReady && !model.registered && model.updatedAt == nil && model.radioDetails.isEmpty)
        precondition(model.messages.count == 1 && model.receiverText.contains("本地记录"))
        await model.call("dial")
        await model.sendSMS()
        precondition(FixtureProtocol.posts == 0 && model.local.records.isEmpty && model.smsText == "离线起草")
        let reopened = AppModel(directory: directory, session: session)
        await reopened.loadLocal()
        precondition(reopened.number == "123" && reopened.smsNumber == "456" && reopened.smsText == "离线起草")

        modem(connected: true, sim:"+CPIN: NOT READY", registered:true)
        await model.refresh()
        precondition(model.statusTitle == "SIM 未就绪" && !model.canSend)
        modem(connected: true, registration:"+CREG: 0,0\n+CEREG: 2,2")
        await model.refresh()
        precondition(model.statusTitle == "正在搜网" && !model.canSend)
        modem(connected: true, registration:"+CREG: 0,3\n+CEREG: 2,0")
        await model.refresh()
        precondition(model.statusTitle == "网络拒绝注册" && !model.canSend)
        modem(connected: true)
        await model.refresh()
        precondition(model.statusTitle == "尚未注册网络" && !model.canSend)

        modem(connected: true, sim:"AT+CPIN?\r\n+CPIN: READY\r\nOK", registration:"+CREG: 0,1\r\nOK\r\n+CEREG: 2,1,\"1234\"", registered: true)
        model.input = "Mic"; model.inputs = [AudioDevice(name: "Mic", sampleRate: 8000)]
        model.output = "Speaker"; model.outputs = [AudioDevice(name: "Speaker", sampleRate: 8000)]
        model.audioSaved = true
        FixtureProtocol.routes["/api/inbox"] = (500, [:])
        FixtureProtocol.routes["/api/notifications/events"] = (500, [:])
        FixtureProtocol.routes["/api/notifications/settings"] = (500, [:])
        await model.refresh(all: true)
        precondition(model.statusTitle == "网络已注册" && model.canDial && model.canSend)
        precondition(model.messages.count == 1 && model.receiverText.contains("刷新失败"))
        precondition(!model.eventsError.isEmpty && !model.mailStatusKnown)
        FixtureProtocol.routes["/api/desktop/call"] = (500, [:])
        await model.refresh(all: true)
        precondition(!model.canDial && model.canSend && model.serviceReady)

        let submission = Task { await model.sendSMS() }
        for _ in 0..<200 where FixtureProtocol.pendingSMS == nil {
            try await Task.sleep(nanoseconds: 10_000_000)
        }
        guard let pending = FixtureProtocol.pendingSMS else { fatalError("mock submit was never reached") }
        model.smsText = "随后编辑的另一份草稿"
        model.smsNumber = "789"
        model.persist()
        pending.finish(200, ["state":"submitted"])
        await submission.value
        precondition(model.local.records.first?.text == "离线起草" && model.local.records.first?.result == "模组已受理")
        precondition(model.smsText == "随后编辑的另一份草稿" && model.smsNumber == "789")
        precondition(FixtureProtocol.posts == 1)

        FixtureProtocol.routes["/api/status"] = (503, [:])
        await model.refresh(all: true)
        precondition(!model.canSend && !model.canDial && model.statusTitle == "本地服务未连接")
        precondition(model.updatedAt == nil && model.radioDetails.isEmpty && model.messages.count == 1)
        model.finishSavingDraft()
        print("PASS native offline state, registration, archive, action gates, partial failures and draft persistence")
    }
}
