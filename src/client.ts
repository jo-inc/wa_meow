/**
 * HTTP client for communicating with the wa_meow Go server
 */

import QRCode from "qrcode";

export interface SessionStatus {
  connected: boolean;
  logged_in: boolean;
  phone?: string;
}

export interface Chat {
  jid: string;
  name: string;
  is_group: boolean;
}

export interface SendMessageResult {
  id: string;
  timestamp: number;
}

export interface GroupInfo {
  jid: string;
  name: string;
  topic: string;
  created: number;
  creator_jid: string;
  participants: Participant[];
  is_announce: boolean;
  is_locked: boolean;
}

export interface Participant {
  jid: string;
  is_admin: boolean;
  is_super_admin: boolean;
}

export interface MessageEvent {
  type: "message";
  payload: {
    id: string;
    chat_jid: string;
    sender_jid: string;
    sender_name: string;
    text: string;
    timestamp: number;
    is_from_me: boolean;
    media_type?: string;
    media_url?: string;
    mime_type?: string;
    caption?: string;
    latitude?: number;
    longitude?: number;
  };
}

export class WhatsAppClient {
  constructor(private serverUrl: string) {}

  private async request<T>(
    path: string,
    options?: RequestInit
  ): Promise<T> {
    const url = `${this.serverUrl}${path}`;
    const response = await fetch(url, {
      ...options,
      headers: {
        "Content-Type": "application/json",
        ...options?.headers,
      },
    });

    if (!response.ok) {
      const error = await response.json().catch(() => ({}));
      throw new Error(error.error || `HTTP ${response.status}`);
    }

    return response.json();
  }

  async health(): Promise<{ status: string }> {
    return this.request("/health");
  }

  async createSession(userId: number): Promise<{ status: string; user_id: number; phone?: string }> {
    return this.request("/sessions", {
      method: "POST",
      body: JSON.stringify({ user_id: userId }),
    });
  }

  async getStatus(userId: number): Promise<SessionStatus> {
    return this.request(`/sessions/status?user_id=${userId}`);
  }

  async deleteSession(userId: number): Promise<{ status: string }> {
    return this.request(`/sessions/delete?user_id=${userId}`, {
      method: "DELETE",
    });
  }

  async getChats(userId: number): Promise<Chat[]> {
    return this.request(`/chats?user_id=${userId}`);
  }

  async sendMessage(
    userId: number,
    chatJid: string,
    text: string,
    replyTo?: string
  ): Promise<SendMessageResult> {
    return this.request("/messages/send", {
      method: "POST",
      body: JSON.stringify({
        user_id: userId,
        chat_jid: chatJid,
        text,
        reply_to: replyTo,
      }),
    });
  }

  async sendImage(
    userId: number,
    chatJid: string,
    imageB64: string,
    mimeType: string,
    caption?: string
  ): Promise<SendMessageResult> {
    return this.request("/messages/image", {
      method: "POST",
      body: JSON.stringify({
        user_id: userId,
        chat_jid: chatJid,
        image_b64: imageB64,
        mime_type: mimeType,
        caption: caption || "",
      }),
    });
  }

  async sendLocation(
    userId: number,
    chatJid: string,
    latitude: number,
    longitude: number,
    name?: string,
    address?: string
  ): Promise<SendMessageResult> {
    return this.request("/messages/location", {
      method: "POST",
      body: JSON.stringify({
        user_id: userId,
        chat_jid: chatJid,
        latitude,
        longitude,
        name: name || "",
        address: address || "",
      }),
    });
  }

  async setTyping(userId: number, chatJid: string, typing: boolean): Promise<void> {
    await this.request("/messages/typing", {
      method: "POST",
      body: JSON.stringify({
        user_id: userId,
        chat_jid: chatJid,
        typing,
      }),
    });
  }

  async sendReaction(
    userId: number,
    chatJid: string,
    messageId: string,
    emoji: string
  ): Promise<SendMessageResult> {
    return this.request("/messages/react", {
      method: "POST",
      body: JSON.stringify({
        user_id: userId,
        chat_jid: chatJid,
        message_id: messageId,
        emoji,
      }),
    });
  }

  async getGroupInfo(userId: number, groupJid: string): Promise<GroupInfo> {
    return this.request(`/groups/info?user_id=${userId}&group_jid=${encodeURIComponent(groupJid)}`);
  }

  async getGroupParticipants(userId: number, groupJid: string): Promise<Participant[]> {
    return this.request(`/groups/participants?user_id=${userId}&group_jid=${encodeURIComponent(groupJid)}`);
  }

  /**
   * Subscribe to SSE events for a user session.
   * Returns an EventSource that emits 'message' events.
   */
  createEventSource(userId: number): EventSource {
    const url = `${this.serverUrl}/events?user_id=${userId}`;
    return new EventSource(url);
  }

  /**
   * Subscribe to QR code SSE stream for pairing.
   * Returns an EventSource that emits 'qr' and 'success' events.
   */
  createQREventSource(userId: number): EventSource {
    const url = `${this.serverUrl}/sessions/qr?user_id=${userId}`;
    return new EventSource(url);
  }

  /**
   * Start QR login flow and return the first QR code as a data URL.
   * Creates a session if needed, then waits for the first QR code.
   */
  async startQRLogin(
    userId: number,
    opts: { force?: boolean; timeoutMs?: number } = {}
  ): Promise<{ qrDataUrl?: string; message: string; alreadyConnected?: boolean }> {
    // First check status
    const status = await this.getStatus(userId);
    if (status.logged_in && !opts.force) {
      return {
        message: `Already connected as ${status.phone || "unknown"}`,
        alreadyConnected: true,
      };
    }

    // Create session to trigger QR generation
    const session = await this.createSession(userId);
    if (session.status === "connected" && !opts.force) {
      return {
        message: `Already connected as ${session.phone || "unknown"}`,
        alreadyConnected: true,
      };
    }

    // Wait for first QR code via SSE
    return new Promise((resolve) => {
      const timeout = opts.timeoutMs || 30000;
      const es = this.createQREventSource(userId);
      const timer = setTimeout(() => {
        es.close();
        resolve({ message: "Timeout waiting for QR code" });
      }, timeout);

      es.addEventListener("qr", async (event) => {
        clearTimeout(timer);
        es.close();
        try {
          // Render QR code as PNG data URL
          const qrDataUrl = await QRCode.toDataURL(event.data, {
            width: 256,
            margin: 2,
          });
          resolve({
            qrDataUrl,
            message: "Scan this QR code in WhatsApp → Linked Devices",
          });
        } catch {
          resolve({ message: "Failed to render QR code" });
        }
      });

      es.addEventListener("success", () => {
        clearTimeout(timer);
        es.close();
        resolve({ message: "Already logged in", alreadyConnected: true });
      });

      es.onerror = () => {
        clearTimeout(timer);
        es.close();
        resolve({ message: "Connection error" });
      };
    });
  }

  /**
   * Wait for QR login to complete.
   * Polls status until connected or timeout.
   */
  async waitForQRLogin(
    userId: number,
    opts: { timeoutMs?: number } = {}
  ): Promise<{ connected: boolean; message: string }> {
    const timeout = opts.timeoutMs || 120000;
    const start = Date.now();
    const pollInterval = 1000;

    while (Date.now() - start < timeout) {
      const status = await this.getStatus(userId);
      if (status.logged_in) {
        return {
          connected: true,
          message: `WhatsApp linked as ${status.phone || "unknown"}`,
        };
      }
      await new Promise((r) => setTimeout(r, pollInterval));
    }

    return {
      connected: false,
      message: "Timeout waiting for QR scan",
    };
  }
}
