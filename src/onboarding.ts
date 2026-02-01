/**
 * Onboarding adapter for wa_meow WhatsApp channel
 */

import type { WhatsAppClient } from "./client.js";
import qrcode from "qrcode-terminal";

// Types from openclaw/plugin-sdk
export interface OpenClawConfig {
  channels?: {
    wa_meow?: WaMeowConfig;
  };
}

export interface WaMeowConfig {
  enabled?: boolean;
  serverUrl?: string;
  accounts?: Record<string, AccountConfig>;
}

interface AccountConfig {
  userId: number;
  enabled?: boolean;
}

interface DmPolicy {
  policy?: "open" | "allowlist" | "pairing" | "disabled";
  allowFrom?: string[];
}

export interface ChannelOnboardingStatus {
  channel: string;
  configured: boolean;
  statusLines: string[];
  selectionHint?: string;
  quickstartScore?: number;
}

export interface ChannelOnboardingStatusContext {
  cfg: OpenClawConfig;
  options?: unknown;
  accountOverrides: Partial<Record<string, string>>;
}

export interface ChannelOnboardingConfigureContext {
  cfg: OpenClawConfig;
  runtime: unknown;
  prompter: WizardPrompter;
  options?: unknown;
  accountOverrides: Partial<Record<string, string>>;
  shouldPromptAccountIds: boolean;
  forceAllowFrom: boolean;
}

export interface ChannelOnboardingResult {
  cfg: OpenClawConfig;
  accountId?: string;
}

export interface ChannelOnboardingDmPolicy {
  label: string;
  channel: string;
  policyKey: string;
  allowFromKey: string;
  getCurrent: (cfg: OpenClawConfig) => string;
  setPolicy: (cfg: OpenClawConfig, policy: string) => OpenClawConfig;
  promptAllowFrom?: (params: {
    cfg: OpenClawConfig;
    prompter: WizardPrompter;
    accountId?: string;
  }) => Promise<OpenClawConfig>;
}

export interface ChannelOnboardingAdapter {
  channel: string;
  getStatus: (ctx: ChannelOnboardingStatusContext) => Promise<ChannelOnboardingStatus>;
  configure: (ctx: ChannelOnboardingConfigureContext) => Promise<ChannelOnboardingResult>;
  dmPolicy?: ChannelOnboardingDmPolicy;
  onAccountRecorded?: (accountId: string, options?: unknown) => void;
  disable?: (cfg: OpenClawConfig) => OpenClawConfig;
}

interface WizardPrompter {
  note(message: string, title?: string): Promise<void>;
  confirm(opts: { message: string; initialValue?: boolean }): Promise<boolean>;
  text(opts: {
    message: string;
    initialValue?: string;
    placeholder?: string;
    validate?: (value: string) => string | undefined;
  }): Promise<string>;
  select<T>(opts: {
    message: string;
    options: { value: T; label: string }[];
  }): Promise<T>;
}

const channel = "wa_meow" as const;

function getDefaultAccountConfig(cfg: OpenClawConfig): AccountConfig | undefined {
  return cfg.channels?.wa_meow?.accounts?.["default"];
}

function setWaMeowEnabled(cfg: OpenClawConfig, enabled: boolean): OpenClawConfig {
  return {
    ...cfg,
    channels: {
      ...cfg.channels,
      wa_meow: {
        ...cfg.channels?.wa_meow,
        enabled,
      },
    },
  };
}

function setWaMeowServerUrl(cfg: OpenClawConfig, serverUrl: string): OpenClawConfig {
  return {
    ...cfg,
    channels: {
      ...cfg.channels,
      wa_meow: {
        ...cfg.channels?.wa_meow,
        serverUrl,
      },
    },
  };
}

function setWaMeowAccount(
  cfg: OpenClawConfig,
  accountId: string,
  account: AccountConfig
): OpenClawConfig {
  return {
    ...cfg,
    channels: {
      ...cfg.channels,
      wa_meow: {
        ...cfg.channels?.wa_meow,
        accounts: {
          ...cfg.channels?.wa_meow?.accounts,
          [accountId]: account,
        },
      },
    },
  };
}

export function createWaMeowOnboardingAdapter(
  client: WhatsAppClient
): ChannelOnboardingAdapter {
  return {
    channel,

    getStatus: async ({ cfg }) => {
      const waMeowConfig = cfg.channels?.wa_meow;
      const enabled = waMeowConfig?.enabled !== false;
      const defaultAccount = getDefaultAccountConfig(cfg);
      const hasAccount = Boolean(defaultAccount?.userId);

      // Try to check if connected
      let connected = false;
      let phone: string | undefined;
      if (hasAccount && enabled) {
        try {
          const status = await client.getStatus(defaultAccount!.userId);
          connected = status.logged_in;
          phone = status.phone;
        } catch {
          // Server not running or error
        }
      }

      const statusLines: string[] = [];
      if (!enabled) {
        statusLines.push("wa_meow: disabled");
      } else if (!hasAccount) {
        statusLines.push("wa_meow: needs configuration");
      } else if (connected) {
        statusLines.push(`wa_meow: connected as ${phone || "unknown"}`);
      } else {
        statusLines.push("wa_meow: configured, needs QR login");
      }

      return {
        channel,
        configured: hasAccount && enabled,
        statusLines,
        selectionHint: connected
          ? `connected (${phone})`
          : hasAccount
            ? "needs QR scan"
            : "needs setup",
      };
    },

    configure: async ({ cfg, prompter }) => {
      let next = cfg;

      await prompter.note(
        [
          "WhatsApp (wa_meow) uses a Go backend with whatsmeow.",
          "The server binary is bundled and starts automatically.",
          "You'll need to scan a QR code to link your WhatsApp account.",
        ].join("\n"),
        "WhatsApp setup"
      );

      // Check existing config
      const existing = cfg.channels?.wa_meow;
      const serverUrl = existing?.serverUrl || "http://localhost:8090";

      // Prompt for server URL
      const newServerUrl = await prompter.text({
        message: "Go server URL",
        initialValue: serverUrl,
        placeholder: "http://localhost:8090",
        validate: (value) => {
          const trimmed = value?.trim();
          if (!trimmed) return "Required";
          try {
            new URL(trimmed);
            return undefined;
          } catch {
            return "Invalid URL";
          }
        },
      });

      next = setWaMeowServerUrl(next, newServerUrl.trim());

      // Prompt for user ID
      const existingUserId = existing?.accounts?.["default"]?.userId;
      const userIdStr = await prompter.text({
        message: "User ID for this WhatsApp account",
        initialValue: existingUserId?.toString() || "1",
        validate: (value) => {
          const num = parseInt(value?.trim() || "", 10);
          if (isNaN(num) || num < 1) return "Must be a positive integer";
          return undefined;
        },
      });
      const userId = parseInt(userIdStr.trim(), 10);

      next = setWaMeowAccount(next, "default", {
        userId,
        enabled: true,
      });

      next = setWaMeowEnabled(next, true);

      // Offer to do QR login now
      const doQrNow = await prompter.confirm({
        message: "Scan QR code now to link WhatsApp?",
        initialValue: true,
      });

      if (doQrNow) {
        // First verify the Go server is running
        try {
          await client.health();
        } catch (err) {
          await prompter.note(
            `Go server not reachable at ${newServerUrl.trim()}.\n\n` +
            `Make sure the wa_meow Go server is running, or start it with:\n` +
            `  openclaw gateway start\n\n` +
            `Error: ${err instanceof Error ? err.message : String(err)}`,
            "WhatsApp Error"
          );
          return { cfg: next, accountId: "default" };
        }

        await prompter.note(
          "Starting QR login flow. A QR code will be displayed.",
          "WhatsApp QR Login"
        );

        try {
          // Check if already connected first
          let forceRelink = false;
          try {
            const status = await client.getStatus(userId);
            if (status.logged_in) {
              const relink = await prompter.confirm({
                message: `Already connected as ${status.phone || "unknown"}. Re-link with new QR code?`,
                initialValue: false,
              });
              if (!relink) {
                await prompter.note(
                  `Keeping existing connection as ${status.phone || "unknown"}.`,
                  "WhatsApp"
                );
                return { cfg: next, accountId: "default" };
              }
              forceRelink = true;
            }
          } catch {
            // Server not running or status check failed, continue with login
          }

          let result = await client.startQRLogin(userId, { 
            timeoutMs: 60000,
            force: forceRelink,
          });

          // If already connected and we didn't explicitly force, ask user
          if (result.alreadyConnected && !forceRelink) {
            const relink = await prompter.confirm({
              message: `${result.message}. Re-link with new QR code?`,
              initialValue: false,
            });
            if (!relink) {
              await prompter.note("Keeping existing connection.", "WhatsApp");
              return { cfg: next, accountId: "default" };
            }
            // Force re-link
            result = await client.startQRLogin(userId, {
              timeoutMs: 60000,
              force: true,
            });
          }

          if (result.qrCode) {
            // Display QR code in terminal
            console.log("\nScan this QR code in WhatsApp → Linked Devices:");
            qrcode.generate(result.qrCode, { small: true });

            // Wait for scan
            await prompter.note(
              "Waiting for QR code scan (timeout: 2 minutes)...",
              "WhatsApp"
            );

            const waitResult = await client.waitForQRLogin(userId, {
              timeoutMs: 120000,
            });

            if (waitResult.connected) {
              await prompter.note(waitResult.message, "WhatsApp Connected");
            } else {
              await prompter.note(
                waitResult.message + "\n\nYou can try again later with 'openclaw gateway'.",
                "WhatsApp"
              );
            }
          } else {
            await prompter.note(result.message, "WhatsApp");
          }
        } catch (err) {
          await prompter.note(
            `QR login failed: ${err instanceof Error ? err.message : String(err)}\n\nYou can try again later with 'openclaw gateway'.`,
            "WhatsApp Error"
          );
        }
      } else {
        await prompter.note(
          "You can link WhatsApp later using 'openclaw gateway'.",
          "WhatsApp"
        );
      }

      return { cfg: next, accountId: "default" };
    },

    disable: (cfg) => setWaMeowEnabled(cfg, false),
  };
}
