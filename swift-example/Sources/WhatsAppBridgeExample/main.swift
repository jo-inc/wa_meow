import Foundation

print("🟢 WhatsApp Bridge Swift Example")
print("================================\n")

let bridge = WhatsAppBridge.shared

// Initialize with database path
let dbPath = FileManager.default.currentDirectoryPath + "/whatsapp_swift.db"
print("📁 Database: \(dbPath)")

guard bridge.initialize(dbPath: dbPath) else {
    print("❌ Failed to initialize WhatsApp bridge")
    exit(1)
}
print("✅ Initialized\n")

// Connect
print("🔌 Connecting...")
let connectResult = bridge.connect()

switch connectResult {
case .connected:
    print("✅ Already connected!\n")
    
case .needsQR:
    print("📱 Scan this QR code with WhatsApp:\n")
    
    // Poll for QR code
    var loggedIn = false
    while !loggedIn {
        switch bridge.getQRCode(timeoutMs: 60000) {
        case .code(let qrCode):
            // Print QR code as ASCII (simplified - use qrterminal for real QR)
            print("QR Code: \(qrCode.prefix(50))...")
            print("\n⚠️  Use a QR library to display this. Waiting for scan...\n")
            
        case .loggedIn:
            print("✅ Logged in successfully!\n")
            loggedIn = true
            
        case .timeout:
            print("⏳ Waiting for QR scan...")
        }
    }
    
case .error(let msg):
    print("❌ Connection error: \(msg)")
    exit(1)
}

// Wait for connection to stabilize
Thread.sleep(forTimeInterval: 2)

// Main REPL loop
print("📱 WhatsApp CLI Ready")
print("Commands: chats, send <jid> <message>, status, quit\n")

while true {
    print("> ", terminator: "")
    guard let input = readLine()?.trimmingCharacters(in: .whitespaces), !input.isEmpty else {
        continue
    }
    
    let parts = input.components(separatedBy: " ")
    let cmd = parts[0].lowercased()
    
    switch cmd {
    case "chats", "list":
        let chats = bridge.getChats()
        print("\n📋 \(chats.count) chats:")
        for (i, chat) in chats.enumerated() {
            let icon = chat.isGroup ? "👥" : "👤"
            print("  \(i+1). \(icon) \(chat.name) [\(chat.jid)]")
        }
        print()
        
    case "send":
        if parts.count < 3 {
            print("Usage: send <jid> <message>")
            continue
        }
        let jid = parts[1]
        let message = parts.dropFirst(2).joined(separator: " ")
        
        let result = bridge.sendMessage(to: jid, text: message)
        if let error = result.error {
            print("❌ Error: \(error)")
        } else {
            print("✅ Sent! ID: \(result.id ?? "unknown")")
        }
        
    case "status":
        print("Connected: \(bridge.isConnected)")
        print("Logged In: \(bridge.isLoggedIn)")
        
    case "quit", "exit":
        print("👋 Disconnecting...")
        bridge.disconnect()
        exit(0)
        
    default:
        print("Unknown command. Try: chats, send, status, quit")
    }
}
