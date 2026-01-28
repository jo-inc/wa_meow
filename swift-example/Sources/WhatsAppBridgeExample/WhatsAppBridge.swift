import Foundation
import CWhatsApp

public struct WhatsAppChat: Codable {
    public let jid: String
    public let name: String
    public let isGroup: Bool
    
    enum CodingKeys: String, CodingKey {
        case jid
        case name
        case isGroup = "is_group"
    }
}

public struct WhatsAppMessage: Codable {
    public let id: String
    public let chatJid: String
    public let senderJid: String
    public let senderName: String
    public let text: String
    public let timestamp: Int64
    public let isFromMe: Bool
    
    enum CodingKeys: String, CodingKey {
        case id
        case chatJid = "chat_jid"
        case senderJid = "sender_jid"
        case senderName = "sender_name"
        case text
        case timestamp
        case isFromMe = "is_from_me"
    }
}

public struct SendResult: Codable {
    public let id: String?
    public let timestamp: Int64?
    public let error: String?
}

public class WhatsAppBridge {
    public static let shared = WhatsAppBridge()
    
    private init() {}
    
    private func parseResult(_ cString: UnsafeMutablePointer<CChar>?) -> [String: Any]? {
        guard let cString = cString else { return nil }
        defer { WhatsAppFreeString(cString) }
        
        let str = String(cString: cString)
        guard let data = str.data(using: .utf8),
              let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            return nil
        }
        return json
    }
    
    public func initialize(dbPath: String) -> Bool {
        let result = dbPath.withCString { cstr in
            parseResult(WhatsAppInit(UnsafeMutablePointer(mutating: cstr)))
        }
        return (result?["status"] as? String) == "initialized"
    }
    
    public enum ConnectResult {
        case connected
        case needsQR
        case error(String)
    }
    
    public func connect() -> ConnectResult {
        guard let result = parseResult(WhatsAppConnect()) else {
            return .error("Failed to parse response")
        }
        
        if let error = result["error"] as? String {
            return .error(error)
        }
        
        switch result["status"] as? String {
        case "connected":
            return .connected
        case "needs_qr":
            return .needsQR
        default:
            return .error("Unknown status")
        }
    }
    
    public enum QRResult {
        case code(String)
        case loggedIn
        case timeout
    }
    
    public func getQRCode(timeoutMs: Int32 = 30000) -> QRResult {
        guard let result = parseResult(WhatsAppGetQRCode(CInt(timeoutMs))) else {
            return .timeout
        }
        
        if let code = result["qr_code"] as? String {
            return .code(code)
        }
        
        if result["status"] as? String == "logged_in" {
            return .loggedIn
        }
        
        return .timeout
    }
    
    public var isConnected: Bool {
        return WhatsAppIsConnected() == 1
    }
    
    public var isLoggedIn: Bool {
        return WhatsAppIsLoggedIn() == 1
    }
    
    public func getChats() -> [WhatsAppChat] {
        guard let cString = WhatsAppGetChats() else { return [] }
        defer { WhatsAppFreeString(cString) }
        
        let str = String(cString: cString)
        guard let data = str.data(using: .utf8) else { return [] }
        
        do {
            return try JSONDecoder().decode([WhatsAppChat].self, from: data)
        } catch {
            print("Failed to decode chats: \(error)")
            return []
        }
    }
    
    public func sendMessage(to jid: String, text: String) -> SendResult {
        let cString = jid.withCString { jidCStr in
            text.withCString { textCStr in
                WhatsAppSendMessage(
                    UnsafeMutablePointer(mutating: jidCStr),
                    UnsafeMutablePointer(mutating: textCStr)
                )
            }
        }
        
        guard let cString = cString else {
            return SendResult(id: nil, timestamp: nil, error: "Failed to send")
        }
        defer { WhatsAppFreeString(cString) }
        
        let str = String(cString: cString)
        guard let data = str.data(using: String.Encoding.utf8) else {
            return SendResult(id: nil, timestamp: nil, error: "Failed to parse response")
        }
        
        do {
            return try JSONDecoder().decode(SendResult.self, from: data)
        } catch {
            return SendResult(id: nil, timestamp: nil, error: "Decode error: \(error)")
        }
    }
    
    public func disconnect() {
        WhatsAppDisconnect()
    }
}
