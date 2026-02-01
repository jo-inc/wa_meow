import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { WhatsAppClient } from "./client.js";

const mockFetch = vi.fn();
vi.stubGlobal("fetch", mockFetch);

describe("WhatsAppClient", () => {
  let client: WhatsAppClient;
  const serverUrl = "http://localhost:8090";

  beforeEach(() => {
    client = new WhatsAppClient(serverUrl);
    mockFetch.mockReset();
  });

  afterEach(() => {
    vi.clearAllMocks();
  });

  function mockResponse(data: unknown, ok = true, status = 200) {
    mockFetch.mockResolvedValueOnce({
      ok,
      status,
      json: () => Promise.resolve(data),
    });
  }

  function mockErrorResponse(error: string, status = 400) {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status,
      json: () => Promise.resolve({ error }),
    });
  }

  describe("health()", () => {
    it("should call /health endpoint", async () => {
      mockResponse({ status: "ok" });

      const result = await client.health();

      expect(mockFetch).toHaveBeenCalledWith(
        `${serverUrl}/health`,
        expect.objectContaining({
          headers: { "Content-Type": "application/json" },
        })
      );
      expect(result).toEqual({ status: "ok" });
    });

    it("should throw on error response", async () => {
      mockErrorResponse("server error", 500);

      await expect(client.health()).rejects.toThrow("server error");
    });
  });

  describe("createSession()", () => {
    it("should POST to /sessions with userId", async () => {
      mockResponse({ status: "created", user_id: 123 });

      const result = await client.createSession(123);

      expect(mockFetch).toHaveBeenCalledWith(
        `${serverUrl}/sessions`,
        expect.objectContaining({
          method: "POST",
          body: JSON.stringify({ user_id: 123 }),
          headers: { "Content-Type": "application/json" },
        })
      );
      expect(result).toEqual({ status: "created", user_id: 123 });
    });

    it("should return phone when connected", async () => {
      mockResponse({ status: "connected", user_id: 123, phone: "+1234567890" });

      const result = await client.createSession(123);

      expect(result.phone).toBe("+1234567890");
    });
  });

  describe("getStatus()", () => {
    it("should call /sessions/status with query param", async () => {
      mockResponse({ connected: true, logged_in: true, phone: "+1234567890" });

      const result = await client.getStatus(123);

      expect(mockFetch).toHaveBeenCalledWith(
        `${serverUrl}/sessions/status?user_id=123`,
        expect.objectContaining({
          headers: { "Content-Type": "application/json" },
        })
      );
      expect(result).toEqual({
        connected: true,
        logged_in: true,
        phone: "+1234567890",
      });
    });

    it("should return disconnected status", async () => {
      mockResponse({ connected: false, logged_in: false });

      const result = await client.getStatus(999);

      expect(result.connected).toBe(false);
      expect(result.logged_in).toBe(false);
    });
  });

  describe("deleteSession()", () => {
    it("should DELETE to /sessions/delete", async () => {
      mockResponse({ status: "deleted" });

      const result = await client.deleteSession(123);

      expect(mockFetch).toHaveBeenCalledWith(
        `${serverUrl}/sessions/delete?user_id=123`,
        expect.objectContaining({
          method: "DELETE",
          headers: { "Content-Type": "application/json" },
        })
      );
      expect(result).toEqual({ status: "deleted" });
    });
  });

  describe("sendMessage()", () => {
    it("should POST to /messages/send", async () => {
      mockResponse({ id: "msg-123", timestamp: 1700000000 });

      const result = await client.sendMessage(
        123,
        "1234567890@s.whatsapp.net",
        "Hello!"
      );

      expect(mockFetch).toHaveBeenCalledWith(
        `${serverUrl}/messages/send`,
        expect.objectContaining({
          method: "POST",
          body: JSON.stringify({
            user_id: 123,
            chat_jid: "1234567890@s.whatsapp.net",
            text: "Hello!",
            reply_to: undefined,
          }),
          headers: { "Content-Type": "application/json" },
        })
      );
      expect(result).toEqual({ id: "msg-123", timestamp: 1700000000 });
    });

    it("should include replyTo when provided", async () => {
      mockResponse({ id: "msg-456", timestamp: 1700000001 });

      await client.sendMessage(
        123,
        "1234567890@s.whatsapp.net",
        "Reply",
        "original-msg-id"
      );

      expect(mockFetch).toHaveBeenCalledWith(
        `${serverUrl}/messages/send`,
        expect.objectContaining({
          body: JSON.stringify({
            user_id: 123,
            chat_jid: "1234567890@s.whatsapp.net",
            text: "Reply",
            reply_to: "original-msg-id",
          }),
        })
      );
    });
  });

  describe("sendImage()", () => {
    it("should POST to /messages/image", async () => {
      mockResponse({ id: "img-123", timestamp: 1700000000 });

      const result = await client.sendImage(
        123,
        "1234567890@s.whatsapp.net",
        "base64imagedata",
        "image/png",
        "Check this out"
      );

      expect(mockFetch).toHaveBeenCalledWith(
        `${serverUrl}/messages/image`,
        expect.objectContaining({
          method: "POST",
          body: JSON.stringify({
            user_id: 123,
            chat_jid: "1234567890@s.whatsapp.net",
            image_b64: "base64imagedata",
            mime_type: "image/png",
            caption: "Check this out",
          }),
          headers: { "Content-Type": "application/json" },
        })
      );
      expect(result).toEqual({ id: "img-123", timestamp: 1700000000 });
    });

    it("should send empty caption when not provided", async () => {
      mockResponse({ id: "img-456", timestamp: 1700000001 });

      await client.sendImage(
        123,
        "1234567890@s.whatsapp.net",
        "base64imagedata",
        "image/jpeg"
      );

      expect(mockFetch).toHaveBeenCalledWith(
        `${serverUrl}/messages/image`,
        expect.objectContaining({
          body: JSON.stringify({
            user_id: 123,
            chat_jid: "1234567890@s.whatsapp.net",
            image_b64: "base64imagedata",
            mime_type: "image/jpeg",
            caption: "",
          }),
        })
      );
    });
  });

  describe("sendLocation()", () => {
    it("should POST to /messages/location with all fields", async () => {
      mockResponse({ id: "loc-123", timestamp: 1700000000 });

      const result = await client.sendLocation(
        123,
        "1234567890@s.whatsapp.net",
        37.7749,
        -122.4194,
        "San Francisco",
        "123 Main St"
      );

      expect(mockFetch).toHaveBeenCalledWith(
        `${serverUrl}/messages/location`,
        expect.objectContaining({
          method: "POST",
          body: JSON.stringify({
            user_id: 123,
            chat_jid: "1234567890@s.whatsapp.net",
            latitude: 37.7749,
            longitude: -122.4194,
            name: "San Francisco",
            address: "123 Main St",
          }),
          headers: { "Content-Type": "application/json" },
        })
      );
      expect(result).toEqual({ id: "loc-123", timestamp: 1700000000 });
    });

    it("should send empty name/address when not provided", async () => {
      mockResponse({ id: "loc-456", timestamp: 1700000001 });

      await client.sendLocation(123, "1234567890@s.whatsapp.net", 40.7128, -74.006);

      expect(mockFetch).toHaveBeenCalledWith(
        `${serverUrl}/messages/location`,
        expect.objectContaining({
          body: JSON.stringify({
            user_id: 123,
            chat_jid: "1234567890@s.whatsapp.net",
            latitude: 40.7128,
            longitude: -74.006,
            name: "",
            address: "",
          }),
        })
      );
    });
  });

  describe("setTyping()", () => {
    it("should POST to /messages/typing with typing=true", async () => {
      mockResponse({});

      await client.setTyping(123, "1234567890@s.whatsapp.net", true);

      expect(mockFetch).toHaveBeenCalledWith(
        `${serverUrl}/messages/typing`,
        expect.objectContaining({
          method: "POST",
          body: JSON.stringify({
            user_id: 123,
            chat_jid: "1234567890@s.whatsapp.net",
            typing: true,
          }),
          headers: { "Content-Type": "application/json" },
        })
      );
    });

    it("should POST with typing=false", async () => {
      mockResponse({});

      await client.setTyping(123, "1234567890@s.whatsapp.net", false);

      expect(mockFetch).toHaveBeenCalledWith(
        `${serverUrl}/messages/typing`,
        expect.objectContaining({
          body: JSON.stringify({
            user_id: 123,
            chat_jid: "1234567890@s.whatsapp.net",
            typing: false,
          }),
        })
      );
    });
  });

  describe("sendReaction()", () => {
    it("should POST to /messages/react", async () => {
      mockResponse({ id: "react-123", timestamp: 1700000000 });

      const result = await client.sendReaction(
        123,
        "1234567890@s.whatsapp.net",
        "msg-to-react",
        "👍"
      );

      expect(mockFetch).toHaveBeenCalledWith(
        `${serverUrl}/messages/react`,
        expect.objectContaining({
          method: "POST",
          body: JSON.stringify({
            user_id: 123,
            chat_jid: "1234567890@s.whatsapp.net",
            message_id: "msg-to-react",
            emoji: "👍",
          }),
          headers: { "Content-Type": "application/json" },
        })
      );
      expect(result).toEqual({ id: "react-123", timestamp: 1700000000 });
    });
  });

  describe("getGroupInfo()", () => {
    it("should call /groups/info with encoded groupJid", async () => {
      const groupInfo = {
        jid: "123456789@g.us",
        name: "Test Group",
        topic: "Group topic",
        created: 1600000000,
        creator_jid: "111@s.whatsapp.net",
        participants: [],
        is_announce: false,
        is_locked: false,
      };
      mockResponse(groupInfo);

      const result = await client.getGroupInfo(123, "123456789@g.us");

      expect(mockFetch).toHaveBeenCalledWith(
        `${serverUrl}/groups/info?user_id=123&group_jid=123456789%40g.us`,
        expect.objectContaining({
          headers: { "Content-Type": "application/json" },
        })
      );
      expect(result).toEqual(groupInfo);
    });
  });

  describe("getGroupParticipants()", () => {
    it("should call /groups/participants", async () => {
      const participants = [
        { jid: "111@s.whatsapp.net", is_admin: true, is_super_admin: true },
        { jid: "222@s.whatsapp.net", is_admin: false, is_super_admin: false },
      ];
      mockResponse(participants);

      const result = await client.getGroupParticipants(123, "123456789@g.us");

      expect(mockFetch).toHaveBeenCalledWith(
        `${serverUrl}/groups/participants?user_id=123&group_jid=123456789%40g.us`,
        expect.objectContaining({
          headers: { "Content-Type": "application/json" },
        })
      );
      expect(result).toEqual(participants);
    });
  });

  describe("error handling", () => {
    it("should throw error message from response", async () => {
      mockErrorResponse("Session not found", 404);

      await expect(client.getStatus(999)).rejects.toThrow("Session not found");
    });

    it("should throw HTTP status when no error message", async () => {
      mockFetch.mockResolvedValueOnce({
        ok: false,
        status: 500,
        json: () => Promise.reject(new Error("parse error")),
      });

      await expect(client.health()).rejects.toThrow("HTTP 500");
    });
  });

  describe("getChats()", () => {
    it("should call /chats with user_id", async () => {
      const chats = [
        { jid: "111@s.whatsapp.net", name: "Alice", is_group: false },
        { jid: "123@g.us", name: "Team", is_group: true },
      ];
      mockResponse(chats);

      const result = await client.getChats(123);

      expect(mockFetch).toHaveBeenCalledWith(
        `${serverUrl}/chats?user_id=123`,
        expect.objectContaining({
          headers: { "Content-Type": "application/json" },
        })
      );
      expect(result).toEqual(chats);
    });
  });

  describe("createEventSource()", () => {
    it("should create EventSource with correct URL", () => {
      const MockEventSource = vi.fn();
      vi.stubGlobal("EventSource", MockEventSource);

      client.createEventSource(123);

      expect(MockEventSource).toHaveBeenCalledWith(
        `${serverUrl}/events?user_id=123`
      );
    });
  });

  describe("createQREventSource()", () => {
    it("should create EventSource for QR stream", () => {
      const MockEventSource = vi.fn();
      vi.stubGlobal("EventSource", MockEventSource);

      client.createQREventSource(456);

      expect(MockEventSource).toHaveBeenCalledWith(
        `${serverUrl}/sessions/qr?user_id=456`
      );
    });
  });
});
