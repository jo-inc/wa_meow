/**
 * HTTP client for communicating with the wa_meow Go server
 */

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
}
