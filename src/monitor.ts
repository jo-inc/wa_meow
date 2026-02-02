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
      resolveStorePath: (storeConfig: unknown, opts: { agentId?: string }) => string;
      recordSessionMetaFromInbound: (params: SessionMetaParams) => Promise<unknown>;
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
  session?: {
    store?: unknown;
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
  cfg: OpenClawConfig;
  channel: string;
  accountId?: string | null;
  peer?: { kind: string; id: string } | null;
  guildId?: string | null;
  teamId?: string | null;
}

interface RouteResult {
  agentId: string;
  sessionKey: string;
  accountId: string;
}

interface SessionMetaParams {
  storePath: string;
  sessionKey: string;
  ctx: MsgContext;
}

interface ActivityParams {
  channel: string;
  direction: "inbound" | "outbound";
  accountId?: string;
}

export interface MonitorWaMeowOpts {
  runtime: PluginRuntime;
  cfg: OpenClawConfig;
  client: WhatsAppClient;
  accountId: string;
  userId: number;
  abortSignal?: AbortSignal;
  log?: Logger;
  onMessage?: (msg: MessageEvent["payload"]) => void;
}

export async function monitorWaMeowProvider(opts: MonitorWaMeowOpts): Promise<void> {
  const { runtime, cfg, client, accountId, userId, abortSignal } = opts;
  
  const logger: Logger = opts.log ?? {
    info: (msg) => console.log(`[wa_meow] ${msg}`),
    warn: (msg) => console.warn(`[wa_meow] ${msg}`),
    error: (msg) => console.error(`[wa_meow] ${msg}`),
    debug: (msg) => console.debug(`[wa_meow] ${msg}`),
  };
  const logVerbose = (msg: string) => {
    logger.debug(msg);
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

  // Get phone number for self-chat detection
  const getSelfPhone = async (): Promise<string | null> => {
    try {
      const status = await client.getStatus(userId);
      if (status.phone) {
        return status.phone.replace(/\D/g, "");
      }
    } catch {
      // ignore
    }
    return null;
  };

  const selfPhone = await getSelfPhone();
  if (!selfPhone) {
    logger.warn("wa_meow: Could not determine self phone number");
  }

  // Strip device suffix from JID (e.g., "12345:25@lid" -> "12345@lid")
  const stripDeviceSuffix = (jid: string): string => {
    return jid.replace(/:\d+@/, "@");
  };

  // Check if a JID represents self-chat
  // Matches jo_bot logic: is_from_me AND base_jid(chat) == base_jid(sender)
  const isSelfChat = (chatJid: string, senderJid: string, isFromMe: boolean): boolean => {
    // Must be from me
    if (!isFromMe) {
      return false;
    }
    
    // base_jid comparison (strip device suffix)
    const normalizedChat = stripDeviceSuffix(chatJid);
    const normalizedSender = stripDeviceSuffix(senderJid);
    return normalizedChat === normalizedSender;
  };

  // Reconnection state
  let currentEs: EventSource | null = null;
  let reconnectAttempts = 0;
  const MAX_RECONNECT_DELAY = 30000; // 30 seconds max
  const BASE_RECONNECT_DELAY = 1000; // 1 second base

  const getReconnectDelay = (): number => {
    // Exponential backoff with jitter: 1s, 2s, 4s, 8s, ... up to 30s
    const delay = Math.min(BASE_RECONNECT_DELAY * Math.pow(2, reconnectAttempts), MAX_RECONNECT_DELAY);
    const jitter = Math.random() * 500; // Add up to 500ms jitter
    return delay + jitter;
  };

  const createAndConnectEventSource = (): EventSource => {
    const es = client.createEventSource(userId);
    
    es.addEventListener("message", (event) => {
      // Reset reconnect attempts on successful message
      reconnectAttempts = 0;
      
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

    es.onopen = () => {
      reconnectAttempts = 0;
      logger.info(`wa_meow: SSE connection established for user ${userId}`);
    };

    es.onerror = () => {
      logger.error(`wa_meow: SSE connection error`);
      
      // Close the current connection
      es.close();
      
      // Don't reconnect if aborted
      if (abortSignal?.aborted) {
        return;
      }
      
      // Schedule reconnection
      const delay = getReconnectDelay();
      reconnectAttempts++;
      logger.info(`wa_meow: Reconnecting in ${Math.round(delay)}ms (attempt ${reconnectAttempts})`);
      
      setTimeout(() => {
        if (!abortSignal?.aborted) {
          currentEs = createAndConnectEventSource();
        }
      }, delay);
    };

    return es;
  };

  // Create initial SSE event source
  currentEs = createAndConnectEventSource();

  const handleMessage = async (payload: MessageEvent["payload"]) => {
    try {
      // Only process self-chat messages (is_from_me AND chat_jid == sender_jid)
      const selfChat = isSelfChat(payload.chat_jid, payload.sender_jid, payload.is_from_me);
      if (!selfChat) {
        return;
      }

      // In self-chat, is_from_me=true means USER sent the message (process it)
      // We only skip messages that are NOT from the user (e.g., bot's own responses)
      // For now, process all self-chat messages since user sends them all

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
        cfg,
        channel: "wa_meow",
        accountId,
      });

      // Resolve store path for session storage
      const storePath = runtime.channel.session.resolveStorePath(cfg.session?.store, {
        agentId: route.agentId,
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

      // Record session meta
      await runtime.channel.session.recordSessionMetaFromInbound({
        storePath,
        sessionKey: route.sessionKey,
        ctx,
      });

      const finalizedCtx = runtime.channel.reply.finalizeInboundContext(ctx);

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

  // Handle abort
  const cleanup = () => {
    currentEs?.close();
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
