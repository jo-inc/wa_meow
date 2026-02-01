/**
 * OpenClaw channel plugin for WhatsApp via wa_meow (whatsmeow)
 *
 * This plugin connects to a Go-based WhatsApp server that uses
 * the whatsmeow library for WhatsApp Web protocol.
 */

import { WhatsAppClient } from "./client.js";
import { spawn, ChildProcess } from "child_process";
import { existsSync } from "fs";
import { join, dirname } from "path";
import { fileURLToPath } from "url";

// OpenClaw plugin types (minimal subset for channel registration)
interface PluginAPI {
  registerChannel(opts: { plugin: ChannelPlugin }): void;
  runtime: {
    config: Config;
    log: Logger;
  };
}

interface Logger {
  info(msg: string, ...args: unknown[]): void;
  warn(msg: string, ...args: unknown[]): void;
  error(msg: string, ...args: unknown[]): void;
  debug(msg: string, ...args: unknown[]): void;
}

interface Config {
  channels?: {
    "wa_meow"?: JoWhatsAppConfig;
  };
}

interface JoWhatsAppConfig {
  serverUrl?: string;
  accounts?: Record<string, AccountConfig>;
}

interface AccountConfig {
  userId: number;
  enabled?: boolean;
}

interface ChannelPlugin {
  id: string;
  meta: ChannelMeta;
  config: ChannelConfigAdapter;
  capabilities: ChannelCapabilities;
  gatewayMethods?: string[];
  outbound: OutboundAdapter;
  gateway?: GatewayAdapter;
  setup?: SetupAdapter;
  status?: StatusAdapter;
  groups?: GroupsAdapter;
}

interface GroupsAdapter {
  getGroupInfo(ctx: GroupContext): Promise<GroupInfoResult>;
  listParticipants(ctx: GroupContext): Promise<ParticipantResult[]>;
}

interface GroupContext {
  accountId: string;
  groupId: string;
}

interface GroupInfoResult {
  id: string;
  name: string;
  topic?: string;
  createdAt?: number;
  creatorId?: string;
  participantCount: number;
  isAnnounceOnly: boolean;
  isLocked: boolean;
}

interface ParticipantResult {
  id: string;
  isAdmin: boolean;
  isSuperAdmin: boolean;
}

interface ChannelMeta {
  label: string;
  selectionLabel?: string;
  blurb?: string;
  docsPath?: string;
  detailLabel?: string;
  systemImage?: string;
}

interface ChannelConfigAdapter {
  listAccountIds(): string[];
  resolveAccount(accountId: string): ResolvedAccount | undefined;
  defaultAccountId?(): string;
}

interface ResolvedAccount {
  accountId: string;
  label: string;
  enabled: boolean;
}

interface ChannelCapabilities {
  chatTypes: string[];
  supportsMedia: boolean;
  supportsThreads: boolean;
  supportsReactions: boolean;
  supportsTypingIndicator: boolean;
}

interface OutboundAdapter {
  deliveryMode: "push" | "poll";
  sendText(ctx: SendContext, text: string): Promise<SendResult>;
  sendMedia?(ctx: SendContext, media: MediaPayload): Promise<SendResult>;
}

interface SendContext {
  accountId: string;
  chatId: string;
  replyToMessageId?: string;
}

interface SendResult {
  messageId: string;
  timestamp?: number;
}

interface MediaPayload {
  type: "image" | "audio" | "video" | "document";
  data: Buffer | Uint8Array;
  mimeType: string;
  caption?: string;
  filename?: string;
}

interface GatewayAdapter {
  start(accountId: string): Promise<void>;
  stop(accountId: string): Promise<void>;
  loginWithQrStart?(opts: {
    accountId?: string;
    force?: boolean;
    timeoutMs?: number;
    verbose?: boolean;
  }): Promise<{ qrDataUrl?: string; message: string }>;
  loginWithQrWait?(opts: {
    accountId?: string;
    timeoutMs?: number;
  }): Promise<{ connected: boolean; message: string }>;
}

interface SetupAdapter {
  startPairing(accountId: string): AsyncGenerator<SetupStep>;
}

interface SetupStep {
  type: "qr" | "status" | "complete" | "error";
  data?: string;
  message?: string;
}

interface StatusAdapter {
  getStatus(accountId: string): Promise<AccountStatus>;
}

interface AccountStatus {
  connected: boolean;
  loggedIn: boolean;
  phone?: string;
}

// Extended plugin API for inbound messages
interface ExtendedPluginAPI extends PluginAPI {
  runtime: PluginAPI["runtime"] & {
    gateway?: {
      handleInboundMessage?(opts: InboundMessage): Promise<void>;
    };
  };
}

interface InboundMessage {
  channel: string;
  accountId: string;
  chatId: string;
  senderId: string;
  senderName?: string;
  text: string;
  messageId: string;
  timestamp: number;
  isFromMe: boolean;
}

// Plugin state
let client: WhatsAppClient;
let config: JoWhatsAppConfig;
let log: Logger;
let inboundHandler: ((msg: InboundMessage) => Promise<void>) | undefined;
const eventSources = new Map<string, EventSource>();
const pollingIntervals = new Map<string, ReturnType<typeof setInterval>>();
let serverProcess: ChildProcess | null = null;
let pluginDir: string;

/**
 * Get the path to the Go server binary for the current platform
 */
function getServerBinaryPath(): string | null {
  const platform = process.platform;
  const arch = process.arch;
  
  let binaryName: string;
  if (platform === "darwin" && arch === "arm64") {
    binaryName = "wa_meow-darwin-arm64";
  } else if (platform === "darwin" && arch === "x64") {
    binaryName = "wa_meow-darwin-x64";
  } else if (platform === "linux" && arch === "x64") {
    binaryName = "wa_meow-linux-x64";
  } else {
    return null;
  }
  
  const binaryPath = join(pluginDir, "bin", binaryName);
  return existsSync(binaryPath) ? binaryPath : null;
}

/**
 * Start the Go server process
 */
function startServer(port: number): Promise<void> {
  return new Promise((resolve, reject) => {
    if (serverProcess) {
      resolve();
      return;
    }
    
    const binaryPath = getServerBinaryPath();
    if (!binaryPath) {
      reject(new Error(`No server binary for ${process.platform}-${process.arch}`));
      return;
    }
    
    log.info(`Starting Go server from ${binaryPath}`);
    
    const dataDir = join(pluginDir, "data");
    
    serverProcess = spawn(binaryPath, [], {
      env: {
        ...process.env,
        PORT: String(port),
        DATA_DIR: dataDir,
        WHATSAPP_SESSION_KEY: process.env.WHATSAPP_SESSION_KEY || "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=",
      },
      stdio: ["ignore", "pipe", "pipe"],
    });
    
    serverProcess.stdout?.on("data", (data) => {
      log.debug(`[server] ${data.toString().trim()}`);
    });
    
    serverProcess.stderr?.on("data", (data) => {
      log.debug(`[server] ${data.toString().trim()}`);
    });
    
    serverProcess.on("error", (err) => {
      log.error(`Server process error: ${err}`);
      serverProcess = null;
    });
    
    serverProcess.on("exit", (code) => {
      log.info(`Server process exited with code ${code}`);
      serverProcess = null;
    });
    
    // Give the server a moment to start
    setTimeout(() => resolve(), 1000);
  });
}

/**
 * Stop the Go server process
 */
function stopServer(): void {
  if (serverProcess) {
    log.info("Stopping Go server");
    serverProcess.kill("SIGTERM");
    serverProcess = null;
  }
}

/**
 * Get the userId for an accountId from config.
 * Returns 1 for the "default" account if not explicitly configured.
 */
function getUserId(accountId: string): number | undefined {
  const userId = config.accounts?.[accountId]?.userId;
  if (userId !== undefined) return userId;
  if (accountId === "default") return 1;
  return undefined;
}

/**
 * Create the channel plugin object
 */
function createChannelPlugin(): ChannelPlugin {
  return {
    id: "wa_meow",

    meta: {
      label: "WhatsApp (wa_meow)",
      selectionLabel: "WhatsApp (wa_meow)",
      blurb: "WhatsApp channel powered by whatsmeow Go library",
      detailLabel: "WhatsApp (wa_meow)",
      systemImage: "message.fill",
    },

    config: {
      listAccountIds(): string[] {
        if (!config.accounts) return ["default"];
        const ids = Object.keys(config.accounts).filter(
          (id) => config.accounts?.[id]?.enabled !== false
        );
        return ids.length > 0 ? ids : ["default"];
      },

      resolveAccount(accountId: string): ResolvedAccount | undefined {
        const acct = config.accounts?.[accountId];
        if (!acct) {
          // Return a default account if none configured
          if (accountId === "default") {
            return {
              accountId: "default",
              label: "WhatsApp (wa_meow)",
              enabled: true,
            };
          }
          return undefined;
        }

        return {
          accountId,
          label: `WhatsApp (User ${acct.userId})`,
          enabled: acct.enabled !== false,
        };
      },

      defaultAccountId(): string {
        return "default";
      },
    },

    capabilities: {
      chatTypes: ["dm", "group"],
      supportsMedia: true,
      supportsThreads: false,
      supportsReactions: true,
      supportsTypingIndicator: true,
    },

    gatewayMethods: ["web.login.start", "web.login.wait"],

    outbound: {
      deliveryMode: "push",

      async sendText(ctx: SendContext, text: string): Promise<SendResult> {
        const userId = getUserId(ctx.accountId);
        if (!userId) {
          throw new Error(`Unknown account: ${ctx.accountId}`);
        }

        const result = await client.sendMessage(
          userId,
          ctx.chatId,
          text,
          ctx.replyToMessageId
        );

        return {
          messageId: result.id,
          timestamp: result.timestamp,
        };
      },

      async sendMedia(ctx: SendContext, media: MediaPayload): Promise<SendResult> {
        const userId = getUserId(ctx.accountId);
        if (!userId) {
          throw new Error(`Unknown account: ${ctx.accountId}`);
        }

        if (media.type === "image") {
          const b64 = Buffer.from(media.data).toString("base64");
          const result = await client.sendImage(
            userId,
            ctx.chatId,
            b64,
            media.mimeType,
            media.caption
          );
          return {
            messageId: result.id,
            timestamp: result.timestamp,
          };
        }

        throw new Error(`Unsupported media type: ${media.type}`);
      },
    },

    gateway: {
      async start(accountId: string): Promise<void> {
        const userId = getUserId(accountId);
        if (!userId) {
          throw new Error(`Unknown account: ${accountId}`);
        }

        log.info(`Starting gateway for account ${accountId} (userId: ${userId})`);

        // Start the Go server if not running
        const serverUrl = config.serverUrl || "http://localhost:8090";
        const port = parseInt(new URL(serverUrl).port || "8090", 10);
        await startServer(port);

        // Create session on the Go server
        const result = await client.createSession(userId);
        log.info(`Session created: ${result.status}`);

        // Start SSE event listener if not already running
        if (!eventSources.has(accountId)) {
          const es = client.createEventSource(userId);

          es.addEventListener("message", (event) => {
            try {
              const data = JSON.parse(event.data);
              log.debug(`Received event for ${accountId}: ${data.type}`);

              if (data.type === "message" && data.payload && inboundHandler) {
                const payload = data.payload;
                // Skip outgoing messages
                if (payload.is_from_me) return;

                inboundHandler({
                  channel: "wa_meow",
                  accountId,
                  chatId: payload.chat_jid,
                  senderId: payload.sender_jid,
                  senderName: payload.sender_name,
                  text: payload.text || payload.caption || "",
                  messageId: payload.id,
                  timestamp: payload.timestamp,
                  isFromMe: payload.is_from_me,
                }).catch((err) => {
                  log.error(`Failed to handle inbound message: ${err}`);
                });
              }
            } catch (err) {
              log.error(`Failed to parse event: ${err}`);
            }
          });

          es.onerror = (err) => {
            log.error(`EventSource error for ${accountId}: ${err}`);
          };

          eventSources.set(accountId, es);
        }
      },

      async stop(accountId: string): Promise<void> {
        const userId = getUserId(accountId);
        if (!userId) return;

        log.info(`Stopping gateway for account ${accountId}`);

        // Close SSE connection
        const es = eventSources.get(accountId);
        if (es) {
          es.close();
          eventSources.delete(accountId);
        }

        // Clear polling interval if any
        const interval = pollingIntervals.get(accountId);
        if (interval) {
          clearInterval(interval);
          pollingIntervals.delete(accountId);
        }

        // Delete session on Go server
        await client.deleteSession(userId);

        // Stop the server if no more active accounts
        if (eventSources.size === 0) {
          stopServer();
        }
      },

      async loginWithQrStart(opts: {
        accountId?: string;
        force?: boolean;
        timeoutMs?: number;
        verbose?: boolean;
      }): Promise<{ qrDataUrl?: string; message: string }> {
        const accountId = opts.accountId || "default";
        const userId = getUserId(accountId);
        if (!userId) {
          return { message: `Unknown account: ${accountId}` };
        }

        log.info(`Starting QR login for account ${accountId} (userId: ${userId})`);

        // Start the Go server if not running
        const serverUrl = config.serverUrl || "http://localhost:8090";
        const port = parseInt(new URL(serverUrl).port || "8090", 10);
        await startServer(port);

        const result = await client.startQRLogin(userId, {
          force: opts.force,
          timeoutMs: opts.timeoutMs,
        });

        return {
          qrDataUrl: result.qrDataUrl,
          message: result.message,
        };
      },

      async loginWithQrWait(opts: {
        accountId?: string;
        timeoutMs?: number;
      }): Promise<{ connected: boolean; message: string }> {
        const accountId = opts.accountId || "default";
        const userId = getUserId(accountId);
        if (!userId) {
          return { connected: false, message: `Unknown account: ${accountId}` };
        }

        log.info(`Waiting for QR login for account ${accountId}`);

        const result = await client.waitForQRLogin(userId, {
          timeoutMs: opts.timeoutMs,
        });

        return result;
      },
    },

    setup: {
      async *startPairing(accountId: string): AsyncGenerator<SetupStep> {
        const userId = getUserId(accountId);
        if (!userId) {
          yield { type: "error", message: `Unknown account: ${accountId}` };
          return;
        }

        yield { type: "status", message: "Creating session..." };

        // Create session - this triggers QR generation if needed
        const result = await client.createSession(userId);

        if (result.status === "connected") {
          yield { type: "complete", message: `Already connected as ${result.phone}` };
          return;
        }

        if (result.status !== "needs_qr") {
          yield { type: "error", message: `Unexpected status: ${result.status}` };
          return;
        }

        yield { type: "status", message: "Waiting for QR code..." };

        // Listen for QR codes via SSE
        const es = client.createQREventSource(userId);

        try {
          for await (const step of listenToQRStream(es)) {
            yield step;
            if (step.type === "complete" || step.type === "error") {
              break;
            }
          }
        } finally {
          es.close();
        }
      },
    },

    status: {
      async getStatus(accountId: string): Promise<AccountStatus> {
        const userId = getUserId(accountId);
        if (!userId) {
          return { connected: false, loggedIn: false };
        }

        const status = await client.getStatus(userId);
        return {
          connected: status.connected,
          loggedIn: status.logged_in,
          phone: status.phone,
        };
      },
    },

    groups: {
      async getGroupInfo(ctx: GroupContext): Promise<GroupInfoResult> {
        const userId = getUserId(ctx.accountId);
        if (!userId) {
          throw new Error(`Unknown account: ${ctx.accountId}`);
        }

        const info = await client.getGroupInfo(userId, ctx.groupId);
        return {
          id: info.jid,
          name: info.name,
          topic: info.topic,
          createdAt: info.created,
          creatorId: info.creator_jid,
          participantCount: info.participants.length,
          isAnnounceOnly: info.is_announce,
          isLocked: info.is_locked,
        };
      },

      async listParticipants(ctx: GroupContext): Promise<ParticipantResult[]> {
        const userId = getUserId(ctx.accountId);
        if (!userId) {
          throw new Error(`Unknown account: ${ctx.accountId}`);
        }

        const participants = await client.getGroupParticipants(userId, ctx.groupId);
        return participants.map((p) => ({
          id: p.jid,
          isAdmin: p.is_admin,
          isSuperAdmin: p.is_super_admin,
        }));
      },
    },
  };
}

/**
 * Convert SSE events from the QR endpoint to SetupStep generator
 */
async function* listenToQRStream(es: EventSource): AsyncGenerator<SetupStep> {
  const queue: SetupStep[] = [];
  let resolve: (() => void) | null = null;
  let done = false;

  const push = (step: SetupStep) => {
    queue.push(step);
    resolve?.();
  };

  es.addEventListener("qr", (event) => {
    push({ type: "qr", data: event.data });
  });

  es.addEventListener("success", () => {
    push({ type: "complete", message: "Successfully logged in" });
    done = true;
  });

  es.addEventListener("timeout", () => {
    push({ type: "error", message: "QR code expired" });
    done = true;
  });

  es.onerror = () => {
    push({ type: "error", message: "Connection error" });
    done = true;
  };

  while (!done) {
    if (queue.length > 0) {
      yield queue.shift()!;
    } else {
      await new Promise<void>((r) => {
        resolve = r;
      });
    }
  }

  // Drain remaining items
  while (queue.length > 0) {
    yield queue.shift()!;
  }
}

/**
 * Plugin registration function
 */
export function register(api: PluginAPI): void {
  const extApi = api as ExtendedPluginAPI;

  // Determine plugin directory (where binaries are located)
  const __filename = fileURLToPath(import.meta.url);
  const __dirname = dirname(__filename);
  pluginDir = dirname(__dirname); // Go up from dist/ to plugin root

  // Create a fallback logger if runtime.log is not available
  const noopLog: Logger = {
    info: () => {},
    warn: () => {},
    error: () => {},
    debug: () => {},
  };
  log = api.runtime?.log || noopLog;
  config = api.runtime?.config?.channels?.["wa_meow"] || {};

  const serverUrl = config.serverUrl || "http://localhost:8090";
  client = new WhatsAppClient(serverUrl);

  // Capture inbound message handler if available
  inboundHandler = extApi.runtime?.gateway?.handleInboundMessage;

  log.info(`Registering wa_meow channel plugin (server: ${serverUrl})`);

  // Start the Go server immediately on plugin load
  const port = parseInt(new URL(serverUrl).port || "8090", 10);
  startServer(port).catch((err) => {
    log.error(`Failed to start Go server: ${err}`);
  });

  api.registerChannel({
    plugin: createChannelPlugin(),
  });
}

// Export for OpenClaw plugin loader
export default {
  id: "wa_meow",
  name: "WhatsApp (wa_meow)",
  register,
};
