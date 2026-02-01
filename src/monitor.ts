/**
 * Monitor for wa_meow WhatsApp inbound messages
 */

import { EventSource } from "eventsource";
import type { WhatsAppClient, MessageEvent } from "./client.js";

// Minimal types from openclaw/plugin-sdk
// These match the PluginRuntime interface
interface PluginRuntime {
  config: {
    loadConfig: () => OpenClawConfig;
  };
  channel: {
    reply: {
      dispatchReplyFromConfig: (params: DispatchParams) => Promise<DispatchResult>;
      createReplyDispatcherWithTyping: (opts: DispatcherOptions) => DispatcherWithTyping;
      finalizeInboundContext: (ctx: MsgContext) => FinalizedMsgContext;
    };
    routing: {
      resolveAgentRoute: (params: RouteParams) => RouteResult;
    };
    session: {
      recordSessionMetaFromInbound: (params: SessionMetaParams) => Promise<void>;
    };
    activity: {
      record: (params: ActivityParams) => void;
    };
    text: {
      resolveTextChunkLimit: (cfg: OpenClawConfig) => number;
    };
  };
  logging: {
    getChildLogger: (opts: { module: string }) => Logger;
    shouldLogVerbose: () => boolean;
  };
}

interface OpenClawConfig {
  channels?: {
    wa_meow?: WaMeowConfig;
  };
}

interface WaMeowConfig {
  enabled?: boolean;
  serverUrl?: string;
  accounts?: Record<string, { userId: number; enabled?: boolean }>;
}

interface Logger {
  info: (msg: string) => void;
  warn: (msg: string) => void;
  error: (msg: string) => void;
  debug: (msg: string) => void;
}

interface RuntimeEnv {
  log: (...args: unknown[]) => void;
  error: (...args: unknown[]) => void;
}

interface MsgContext {
  Body?: string;
  BodyForAgent?: string;
  RawBody?: string;
  From?: string;
  To?: string;
  SessionKey?: string;
  AccountId?: string;
  MessageSid?: string;
  SenderId?: string;
  SenderName?: string;
  Timestamp?: number;
  Provider?: string;
  OriginatingChannel?: string;
  OriginatingTo?: string;
  ChatType?: string;
}

interface FinalizedMsgContext extends MsgContext {
  CommandAuthorized: boolean;
}

interface DispatchParams {
  ctx: FinalizedMsgContext;
  cfg: OpenClawConfig;
  dispatcher: ReplyDispatcher;
  replyOptions?: Record<string, unknown>;
}

interface DispatchResult {
  queuedFinal: boolean;
  counts: { final: number };
}

interface DispatcherOptions {
  deliver: (payload: ReplyPayload) => Promise<void>;
  onError?: (err: Error, info: { kind: string }) => void;
  onReplyStart?: () => void;
  onIdle?: () => void;
}

interface DispatcherWithTyping {
  dispatcher: ReplyDispatcher;
  replyOptions: Record<string, unknown>;
  markDispatchIdle: () => void;
}

interface ReplyDispatcher {
  deliver: (payload: ReplyPayload) => Promise<void>;
}

interface ReplyPayload {
  text: string;
  blocks?: unknown[];
}

interface RouteParams {
  channel: string;
  accountId: string;
  chatId: string;
  senderId?: string;
  isGroup?: boolean;
}

interface RouteResult {
  sessionKey: string;
  accountId: string;
}

interface SessionMetaParams {
  channel: string;
  accountId: string;
  sessionKey: string;
  senderId?: string;
  senderName?: string;
}

interface ActivityParams {
  channel: string;
  direction: "inbound" | "outbound";
  accountId?: string;
}

export interface MonitorWaMeowOpts {
  runtime: PluginRuntime;
  runtimeEnv?: RuntimeEnv;
  client: WhatsAppClient;
  accountId: string;
  userId: number;
  abortSignal?: AbortSignal;
  onMessage?: (msg: MessageEvent["payload"]) => void;
}

export async function monitorWaMeowProvider(opts: MonitorWaMeowOpts): Promise<void> {
  const { runtime, client, accountId, userId, abortSignal } = opts;
  
  const cfg = runtime.config.loadConfig() as OpenClawConfig;
  const logger = runtime.logging.getChildLogger({ module: "wa_meow-auto-reply" });
  const logVerbose = (msg: string) => {
    if (runtime.logging.shouldLogVerbose()) {
      logger.debug(msg);
    }
  };

  const runtimeEnv: RuntimeEnv = opts.runtimeEnv ?? {
    log: (...args) => logger.info(args.map(String).join(" ")),
    error: (...args) => logger.error(args.map(String).join(" ")),
  };

  // Create/verify session
  try {
    const status = await client.getStatus(userId);
    if (!status.logged_in) {
      logger.warn(`wa_meow: User ${userId} not logged in, skipping monitor`);
      return;
    }
    logger.info(`wa_meow: Monitoring user ${userId} (${status.phone || "unknown"})`);
  } catch (err) {
    logger.error(`wa_meow: Failed to check status: ${err}`);
    return;
  }

  // Self-chat JID pattern
  const getSelfChatJid = async (): Promise<string | null> => {
    try {
      const status = await client.getStatus(userId);
      if (status.phone) {
        // WhatsApp self-chat is your own number@s.whatsapp.net
        const normalized = status.phone.replace(/\D/g, "");
        return `${normalized}@s.whatsapp.net`;
      }
    } catch {
      // ignore
    }
    return null;
  };

  const selfChatJid = await getSelfChatJid();
  if (!selfChatJid) {
    logger.warn("wa_meow: Could not determine self-chat JID");
  }

  // Create SSE event source
  const es = client.createEventSource(userId);

  const handleMessage = async (payload: MessageEvent["payload"]) => {
    try {
      // Only process self-chat messages
      if (selfChatJid && payload.chat_jid !== selfChatJid) {
        logVerbose(`wa_meow: Ignoring non-self-chat message from ${payload.chat_jid}`);
        return;
      }

      // Skip outgoing messages (our own responses)
      if (payload.is_from_me) {
        logVerbose(`wa_meow: Ignoring outgoing message`);
        return;
      }

      const bodyText = payload.text || payload.caption || "";
      if (!bodyText.trim()) {
        logVerbose(`wa_meow: Ignoring empty message`);
        return;
      }

      logger.info(`wa_meow: Received self-chat message: ${bodyText.slice(0, 50)}...`);

      // Record activity
      runtime.channel.activity.record({
        channel: "wa_meow",
        direction: "inbound",
        accountId,
      });

      // Resolve routing
      const route = runtime.channel.routing.resolveAgentRoute({
        channel: "wa_meow",
        accountId,
        chatId: payload.chat_jid,
        senderId: payload.sender_jid,
        isGroup: false,
      });

      // Record session meta
      await runtime.channel.session.recordSessionMetaFromInbound({
        channel: "wa_meow",
        accountId,
        sessionKey: route.sessionKey,
        senderId: payload.sender_jid,
        senderName: payload.sender_name,
      });

      // Build message context
      const ctx: MsgContext = {
        Body: bodyText,
        BodyForAgent: bodyText,
        RawBody: bodyText,
        From: payload.sender_jid,
        To: payload.chat_jid,
        SessionKey: route.sessionKey,
        AccountId: accountId,
        MessageSid: payload.id,
        SenderId: payload.sender_jid,
        SenderName: payload.sender_name || "You",
        Timestamp: payload.timestamp,
        Provider: "wa_meow",
        OriginatingChannel: "wa_meow",
        OriginatingTo: payload.chat_jid,
        ChatType: "dm",
      };

      const finalizedCtx = runtime.channel.reply.finalizeInboundContext(ctx);
      const textLimit = runtime.channel.text.resolveTextChunkLimit(cfg);

      // Create reply dispatcher
      const { dispatcher, replyOptions, markDispatchIdle } =
        runtime.channel.reply.createReplyDispatcherWithTyping({
          deliver: async (replyPayload) => {
            const text = replyPayload.text;
            if (!text?.trim()) return;

            try {
              await client.sendMessage(userId, payload.chat_jid, text);
              logVerbose(`wa_meow: Sent reply to ${payload.chat_jid}`);
              
              runtime.channel.activity.record({
                channel: "wa_meow",
                direction: "outbound",
                accountId,
              });
            } catch (err) {
              logger.error(`wa_meow: Failed to send reply: ${err}`);
            }
          },
          onError: (err, info) => {
            logger.error(`wa_meow: ${info.kind} reply failed: ${err}`);
          },
        });

      // Dispatch to agent
      const { queuedFinal, counts } = await runtime.channel.reply.dispatchReplyFromConfig({
        ctx: finalizedCtx,
        cfg,
        dispatcher,
        replyOptions,
      });

      markDispatchIdle();

      if (queuedFinal) {
        logVerbose(`wa_meow: Delivered ${counts.final} reply(ies)`);
      }
    } catch (err) {
      logger.error(`wa_meow: Handler error: ${err}`);
    }
  };

  // Listen for messages
  es.addEventListener("message", (event) => {
    try {
      const data = JSON.parse(event.data) as MessageEvent;
      if (data.type === "message" && data.payload) {
        opts.onMessage?.(data.payload);
        handleMessage(data.payload).catch((err) => {
          logger.error(`wa_meow: Message handler error: ${err}`);
        });
      }
    } catch (err) {
      logger.error(`wa_meow: Failed to parse SSE event: ${err}`);
    }
  });

  es.onerror = () => {
    logger.error(`wa_meow: SSE connection error`);
  };

  // Handle abort
  const cleanup = () => {
    es.close();
    logger.info(`wa_meow: Monitor stopped for user ${userId}`);
  };

  if (abortSignal) {
    abortSignal.addEventListener("abort", cleanup, { once: true });
  }

  // Keep running until aborted
  return new Promise<void>((resolve) => {
    if (abortSignal) {
      abortSignal.addEventListener("abort", () => resolve(), { once: true });
    }
  });
}
