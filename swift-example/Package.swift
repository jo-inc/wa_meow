// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "WhatsAppBridgeExample",
    platforms: [.macOS(.v14)],
    products: [
        .executable(name: "whatsapp-swift", targets: ["WhatsAppBridgeExample"])
    ],
    targets: [
        .systemLibrary(
            name: "CWhatsApp",
            path: "Sources/CWhatsApp"
        ),
        .executableTarget(
            name: "WhatsAppBridgeExample",
            dependencies: ["CWhatsApp"],
            linkerSettings: [
                .unsafeFlags(["-L", "../", "-lwhatsapp"]),
                .unsafeFlags(["-Xlinker", "-rpath", "-Xlinker", "@executable_path/../"])
            ]
        )
    ]
)
