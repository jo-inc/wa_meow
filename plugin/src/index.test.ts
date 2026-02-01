import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

const mockFetch = vi.fn();
vi.stubGlobal("fetch", mockFetch);

vi.mock("./client.js", () => {
  return {
    WhatsAppClient: vi.fn().mockImplementation(() => ({
      health: vi.fn(),
      createSession: vi.fn(),
      getStatus: vi.fn(),
      deleteSession: vi.fn(),
      sendMessage: vi.fn(),
      sendImage: vi.fn(),
      sendLocation: vi.fn(),
      setTyping: vi.fn(),
      sendReaction: vi.fn(),
      getGroupInfo: vi.fn(),
      getGroupParticipants: vi.fn(),
      createEventSource: vi.fn(),
      createQREventSource: vi.fn(),
    })),
  };
});

import { register } from "./index.js";
import { WhatsAppClient } from "./client.js";

describe("jo-whatsapp plugin", () => {
  let registeredPlugin: {
    id: string;
    meta: {
      label: string;
      selectionLabel?: string;
      blurb?: string;
      detailLabel?: string;
      systemImage?: string;
    };
    config: {
      listAccountIds(): string[];
      resolveAccount(
        accountId: string
      ): { accountId: string; label: string; enabled: boolean } | undefined;
    };
    capabilities: {
      chatTypes: string[];
      supportsMedia: boolean;
      supportsThreads: boolean;
      supportsReactions: boolean;
      supportsTypingIndicator: boolean;
    };
    outbound: {
      deliveryMode: string;
      sendText(
        ctx: { accountId: string; chatId: string; replyToMessageId?: string },
        text: string
      ): Promise<{ messageId: string; timestamp?: number }>;
      sendMedia?(
        ctx: { accountId: string; chatId: string },
        media: {
          type: string;
          data: Buffer | Uint8Array;
          mimeType: string;
          caption?: string;
        }
      ): Promise<{ messageId: string; timestamp?: number }>;
    };
    gateway?: {
      start(accountId: string): Promise<void>;
      stop(accountId: string): Promise<void>;
    };
    status?: {
      getStatus(
        accountId: string
      ): Promise<{ connected: boolean; loggedIn: boolean; phone?: string }>;
    };
    groups?: {
      getGroupInfo(ctx: {
        accountId: string;
        groupId: string;
      }): Promise<{
        id: string;
        name: string;
        topic?: string;
        createdAt?: number;
        creatorId?: string;
        participantCount: number;
        isAnnounceOnly: boolean;
        isLocked: boolean;
      }>;
      listParticipants(ctx: {
        accountId: string;
        groupId: string;
      }): Promise<{ id: string; isAdmin: boolean; isSuperAdmin: boolean }[]>;
    };
  };

  let mockClientInstance: ReturnType<typeof vi.fn> & {
    sendMessage: ReturnType<typeof vi.fn>;
    sendImage: ReturnType<typeof vi.fn>;
    getStatus: ReturnType<typeof vi.fn>;
    createSession: ReturnType<typeof vi.fn>;
    deleteSession: ReturnType<typeof vi.fn>;
    getGroupInfo: ReturnType<typeof vi.fn>;
    getGroupParticipants: ReturnType<typeof vi.fn>;
    createEventSource: ReturnType<typeof vi.fn>;
  };

  const mockLogger = {
    info: vi.fn(),
    warn: vi.fn(),
    error: vi.fn(),
    debug: vi.fn(),
  };

  function createMockAPI(configOverrides = {}) {
    return {
      registerChannel: vi.fn((opts: { plugin: typeof registeredPlugin }) => {
        registeredPlugin = opts.plugin;
      }),
      runtime: {
        config: {
          channels: {
            "jo-whatsapp": {
              serverUrl: "http://test-server:8090",
              accounts: {
                main: { userId: 123, enabled: true },
                secondary: { userId: 456, enabled: true },
                disabled: { userId: 789, enabled: false },
              },
              ...configOverrides,
            },
          },
        },
        log: mockLogger,
      },
    };
  }

  beforeEach(() => {
    vi.clearAllMocks();
    mockClientInstance = vi.mocked(WhatsAppClient).mock.results[0]
      ?.value as typeof mockClientInstance;
  });

  afterEach(() => {
    vi.clearAllMocks();
  });

  describe("register()", () => {
    it("should create WhatsAppClient with configured serverUrl", () => {
      const api = createMockAPI();
      register(api);

      expect(WhatsAppClient).toHaveBeenCalledWith("http://test-server:8090");
    });

    it("should use default serverUrl when not configured", () => {
      const api = {
        registerChannel: vi.fn(),
        runtime: {
          config: { channels: {} },
          log: mockLogger,
        },
      };
      register(api);

      expect(WhatsAppClient).toHaveBeenCalledWith("http://localhost:8090");
    });

    it("should call registerChannel with plugin", () => {
      const api = createMockAPI();
      register(api);

      expect(api.registerChannel).toHaveBeenCalledWith({
        plugin: expect.objectContaining({
          id: "jo-whatsapp",
        }),
      });
    });
  });

  describe("createChannelPlugin()", () => {
    beforeEach(() => {
      const api = createMockAPI();
      register(api);
      mockClientInstance = vi.mocked(WhatsAppClient).mock.results[0]
        ?.value as typeof mockClientInstance;
    });

    describe("meta", () => {
      it("should have correct plugin metadata", () => {
        expect(registeredPlugin.id).toBe("jo-whatsapp");
        expect(registeredPlugin.meta.label).toBe("WhatsApp (whatsmeow)");
        expect(registeredPlugin.meta.selectionLabel).toBe("Jo WhatsApp");
        expect(registeredPlugin.meta.systemImage).toBe("message.fill");
      });
    });

    describe("config.listAccountIds()", () => {
      it("should return enabled account IDs", () => {
        const accountIds = registeredPlugin.config.listAccountIds();

        expect(accountIds).toContain("main");
        expect(accountIds).toContain("secondary");
        expect(accountIds).not.toContain("disabled");
      });

      it("should return empty array when no accounts configured", () => {
        const api = {
          registerChannel: vi.fn((opts: { plugin: typeof registeredPlugin }) => {
            registeredPlugin = opts.plugin;
          }),
          runtime: {
            config: { channels: { "jo-whatsapp": {} } },
            log: mockLogger,
          },
        };
        register(api);

        expect(registeredPlugin.config.listAccountIds()).toEqual([]);
      });
    });

    describe("config.resolveAccount()", () => {
      it("should resolve existing account", () => {
        const account = registeredPlugin.config.resolveAccount("main");

        expect(account).toEqual({
          accountId: "main",
          label: "WhatsApp (User 123)",
          enabled: true,
        });
      });

      it("should return undefined for unknown account", () => {
        const account = registeredPlugin.config.resolveAccount("unknown");

        expect(account).toBeUndefined();
      });

      it("should mark disabled account as not enabled", () => {
        const account = registeredPlugin.config.resolveAccount("disabled");

        expect(account?.enabled).toBe(false);
      });
    });

    describe("capabilities", () => {
      it("should have correct capabilities", () => {
        expect(registeredPlugin.capabilities).toEqual({
          chatTypes: ["dm", "group"],
          supportsMedia: true,
          supportsThreads: false,
          supportsReactions: true,
          supportsTypingIndicator: true,
        });
      });
    });

    describe("outbound.sendText()", () => {
      it("should send text message through client", async () => {
        mockClientInstance.sendMessage.mockResolvedValue({
          id: "msg-123",
          timestamp: 1700000000,
        });

        const result = await registeredPlugin.outbound.sendText(
          { accountId: "main", chatId: "111@s.whatsapp.net" },
          "Hello!"
        );

        expect(mockClientInstance.sendMessage).toHaveBeenCalledWith(
          123,
          "111@s.whatsapp.net",
          "Hello!",
          undefined
        );
        expect(result).toEqual({
          messageId: "msg-123",
          timestamp: 1700000000,
        });
      });

      it("should include replyToMessageId when provided", async () => {
        mockClientInstance.sendMessage.mockResolvedValue({
          id: "msg-456",
          timestamp: 1700000001,
        });

        await registeredPlugin.outbound.sendText(
          {
            accountId: "main",
            chatId: "111@s.whatsapp.net",
            replyToMessageId: "original-id",
          },
          "Reply"
        );

        expect(mockClientInstance.sendMessage).toHaveBeenCalledWith(
          123,
          "111@s.whatsapp.net",
          "Reply",
          "original-id"
        );
      });

      it("should throw for unknown account", async () => {
        await expect(
          registeredPlugin.outbound.sendText(
            { accountId: "unknown", chatId: "111@s.whatsapp.net" },
            "Hello!"
          )
        ).rejects.toThrow("Unknown account: unknown");
      });
    });

    describe("outbound.sendMedia()", () => {
      it("should send image through client", async () => {
        mockClientInstance.sendImage.mockResolvedValue({
          id: "img-123",
          timestamp: 1700000000,
        });

        const imageData = Buffer.from("fake-image-data");
        const result = await registeredPlugin.outbound.sendMedia!(
          { accountId: "main", chatId: "111@s.whatsapp.net" },
          {
            type: "image",
            data: imageData,
            mimeType: "image/png",
            caption: "Check this",
          }
        );

        expect(mockClientInstance.sendImage).toHaveBeenCalledWith(
          123,
          "111@s.whatsapp.net",
          imageData.toString("base64"),
          "image/png",
          "Check this"
        );
        expect(result).toEqual({
          messageId: "img-123",
          timestamp: 1700000000,
        });
      });

      it("should throw for unsupported media type", async () => {
        await expect(
          registeredPlugin.outbound.sendMedia!(
            { accountId: "main", chatId: "111@s.whatsapp.net" },
            {
              type: "video",
              data: Buffer.from("data"),
              mimeType: "video/mp4",
            }
          )
        ).rejects.toThrow("Unsupported media type: video");
      });

      it("should throw for unknown account", async () => {
        await expect(
          registeredPlugin.outbound.sendMedia!(
            { accountId: "unknown", chatId: "111@s.whatsapp.net" },
            {
              type: "image",
              data: Buffer.from("data"),
              mimeType: "image/png",
            }
          )
        ).rejects.toThrow("Unknown account: unknown");
      });
    });

    describe("status.getStatus()", () => {
      it("should return status from client", async () => {
        mockClientInstance.getStatus.mockResolvedValue({
          connected: true,
          logged_in: true,
          phone: "+1234567890",
        });

        const status = await registeredPlugin.status!.getStatus("main");

        expect(mockClientInstance.getStatus).toHaveBeenCalledWith(123);
        expect(status).toEqual({
          connected: true,
          loggedIn: true,
          phone: "+1234567890",
        });
      });

      it("should return disconnected for unknown account", async () => {
        const status = await registeredPlugin.status!.getStatus("unknown");

        expect(status).toEqual({
          connected: false,
          loggedIn: false,
        });
      });
    });

    describe("groups.getGroupInfo()", () => {
      it("should return group info from client", async () => {
        mockClientInstance.getGroupInfo.mockResolvedValue({
          jid: "123@g.us",
          name: "Test Group",
          topic: "Topic",
          created: 1600000000,
          creator_jid: "111@s.whatsapp.net",
          participants: [{ jid: "111@s.whatsapp.net" }, { jid: "222@s.whatsapp.net" }],
          is_announce: true,
          is_locked: false,
        });

        const info = await registeredPlugin.groups!.getGroupInfo({
          accountId: "main",
          groupId: "123@g.us",
        });

        expect(mockClientInstance.getGroupInfo).toHaveBeenCalledWith(123, "123@g.us");
        expect(info).toEqual({
          id: "123@g.us",
          name: "Test Group",
          topic: "Topic",
          createdAt: 1600000000,
          creatorId: "111@s.whatsapp.net",
          participantCount: 2,
          isAnnounceOnly: true,
          isLocked: false,
        });
      });

      it("should throw for unknown account", async () => {
        await expect(
          registeredPlugin.groups!.getGroupInfo({
            accountId: "unknown",
            groupId: "123@g.us",
          })
        ).rejects.toThrow("Unknown account: unknown");
      });
    });

    describe("groups.listParticipants()", () => {
      it("should return participants from client", async () => {
        mockClientInstance.getGroupParticipants.mockResolvedValue([
          { jid: "111@s.whatsapp.net", is_admin: true, is_super_admin: true },
          { jid: "222@s.whatsapp.net", is_admin: false, is_super_admin: false },
        ]);

        const participants = await registeredPlugin.groups!.listParticipants({
          accountId: "main",
          groupId: "123@g.us",
        });

        expect(mockClientInstance.getGroupParticipants).toHaveBeenCalledWith(
          123,
          "123@g.us"
        );
        expect(participants).toEqual([
          { id: "111@s.whatsapp.net", isAdmin: true, isSuperAdmin: true },
          { id: "222@s.whatsapp.net", isAdmin: false, isSuperAdmin: false },
        ]);
      });

      it("should throw for unknown account", async () => {
        await expect(
          registeredPlugin.groups!.listParticipants({
            accountId: "unknown",
            groupId: "123@g.us",
          })
        ).rejects.toThrow("Unknown account: unknown");
      });
    });

    describe("gateway.start()", () => {
      it("should create session and event source", async () => {
        const mockEventSource = {
          addEventListener: vi.fn(),
          onerror: null as ((err: Event) => void) | null,
          close: vi.fn(),
        };
        mockClientInstance.createSession.mockResolvedValue({ status: "created" });
        mockClientInstance.createEventSource.mockReturnValue(mockEventSource);

        await registeredPlugin.gateway!.start("main");

        expect(mockClientInstance.createSession).toHaveBeenCalledWith(123);
        expect(mockClientInstance.createEventSource).toHaveBeenCalledWith(123);
      });

      it("should throw for unknown account", async () => {
        await expect(registeredPlugin.gateway!.start("unknown")).rejects.toThrow(
          "Unknown account: unknown"
        );
      });
    });

    describe("gateway.stop()", () => {
      it("should delete session", async () => {
        mockClientInstance.deleteSession.mockResolvedValue({ status: "deleted" });

        await registeredPlugin.gateway!.stop("main");

        expect(mockClientInstance.deleteSession).toHaveBeenCalledWith(123);
      });

      it("should silently skip unknown account", async () => {
        await expect(registeredPlugin.gateway!.stop("unknown")).resolves.toBeUndefined();
      });
    });
  });

  describe("default export", () => {
    it("should export plugin metadata", async () => {
      const defaultExport = await import("./index.js").then((m) => m.default);

      expect(defaultExport.id).toBe("jo-whatsapp");
      expect(defaultExport.name).toBe("Jo WhatsApp (whatsmeow)");
      expect(typeof defaultExport.register).toBe("function");
    });
  });
});
