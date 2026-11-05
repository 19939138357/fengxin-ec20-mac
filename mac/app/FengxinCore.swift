import Foundation

enum FXRules {
    static func number(_ raw: String) -> String { raw.filter { !$0.isWhitespace && $0 != "-" && $0 != "(" && $0 != ")" } }
    static func validNumber(_ raw: String) -> Bool { number(raw).range(of: "^\\+?[0-9]{3,20}$", options: .regularExpression) != nil }
    static func mail(_ raw: String) -> Bool { raw.trimmingCharacters(in:.whitespacesAndNewlines).range(of: "^[^\\s@]+@[^\\s@]+\\.[^\\s@]+$", options:.regularExpression) != nil }
    static func validSMS(_ text: String) -> Bool { !text.trimmingCharacters(in:.whitespacesAndNewlines).isEmpty && text.utf16.count <= 70 }
    static func signal(_ raw: String) -> String {
        guard let r=raw.range(of:"\\+CSQ: *[0-9]+",options:.regularExpression),let value=Int(raw[r].split(separator:":").last!.trimmingCharacters(in:.whitespaces)),value != 99 else{return "暂无读数"}
        return value <= 7 ? "较弱 · \(value)/31" : (value <= 14 ? "一般 · \(value)/31" : "良好 · \(value)/31")
    }
    static func date(_ raw: String) -> Date? {
        let f=ISO8601DateFormatter(); f.formatOptions=[.withInternetDateTime,.withFractionalSeconds]
        return f.date(from:raw) ?? ISO8601DateFormatter().date(from:raw)
    }
    static func time(_ raw: String) -> String { guard let date=date(raw) else{return raw};let f=DateFormatter();f.locale=Locale(identifier:"zh_CN");f.dateFormat="MM月dd日 HH:mm";return f.string(from:date) }
    static func network(service:Bool,device:Bool,sim:Bool,registered:Bool,starting:Bool) -> String {
        if starting {return "正在连接"};if !service{return "服务未启动"};if !device{return "未连接 EC20"};if !sim{return "SIM 未就绪"};return registered ? "网络已注册":"正在搜网"
    }
}
struct FXContact: Codable,Identifiable { var id=UUID().uuidString;var name:String;var number:String }
struct FXRecord: Codable,Identifiable { var id=UUID().uuidString;var number:String;var kind:String;var result:String;var time:String=ISO8601DateFormatter().string(from:Date());var text:String="" }
struct FXLocal: Codable {
    var number="",smsNumber="",draft=""
    var theme="浅色"
    var readIDs:[String]=[]
    var contacts:[FXContact]=[]
    var records:[FXRecord]=[]
    init() {}
    enum CodingKeys:String,CodingKey {case number,smsNumber,draft,theme,readIDs,contacts,records}
    init(from decoder:Decoder) throws {
        let c=try decoder.container(keyedBy:CodingKeys.self)
        number=try c.decodeIfPresent(String.self,forKey:.number) ?? ""
        smsNumber=try c.decodeIfPresent(String.self,forKey:.smsNumber) ?? ""
        draft=try c.decodeIfPresent(String.self,forKey:.draft) ?? ""
        theme=try c.decodeIfPresent(String.self,forKey:.theme) ?? "浅色"
        readIDs=try c.decodeIfPresent([String].self,forKey:.readIDs) ?? []
        contacts=try c.decodeIfPresent([FXContact].self,forKey:.contacts) ?? []
        records=try c.decodeIfPresent([FXRecord].self,forKey:.records) ?? []
    }
}
