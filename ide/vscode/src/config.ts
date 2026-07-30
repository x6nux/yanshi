// ide/vscode/src/config.ts
//
// Read yanshi.* settings from VS Code workspace configuration and SecretStorage.
// Token NEVER enters settings.json (machine-overridable only as a fallback to
// prompt the user); the canonical store is SecretStorage so it does not leak
// across machines, telemetry, or the VS Code sync surface.

import * as vscode from "vscode";
import type { AgentClientOptions } from "@x6nux/yanshi-sdk";

export interface ExtensionConfig {
  baseUrl: string;
  token: string | undefined;
  streamTransport: "sse" | "ws";
  maxOpenFiles: number;
  maxContextBytes: number;
}

const SECRET_KEY = "yanshi.token";

/**
 * Read the effective extension configuration. Falls back to defaults baked
 * into package.json when the user has not set anything.
 */
export async function readConfig(context: vscode.ExtensionContext): Promise<ExtensionConfig> {
  const config = vscode.workspace.getConfiguration("yanshi");
  const storedToken = await context.secrets.get(SECRET_KEY);
  const maxOpenFiles = clamp(config.get<number>("maxOpenFiles", 8), 0, 16);
  const maxContextBytes = clamp(config.get<number>("maxContextBytes", 32768), 1024, 262144);
  return {
    baseUrl: (config.get<string>("serverUrl", "http://127.0.0.1:8080") || "").replace(/\/$/, ""),
    token: storedToken,
    streamTransport: config.get<"sse" | "ws">("streamTransport", "sse"),
    maxOpenFiles,
    maxContextBytes,
  };
}

export async function saveToken(context: vscode.ExtensionContext, token: string): Promise<void> {
  await context.secrets.store(SECRET_KEY, token);
}

export async function deleteToken(context: vscode.ExtensionContext): Promise<void> {
  await context.secrets.delete(SECRET_KEY);
}

export function clientOptions(config: ExtensionConfig): AgentClientOptions {
  return {
    baseUrl: config.baseUrl,
    token: config.token,
    streamTransport: config.streamTransport,
  };
}

function clamp(value: number, min: number, max: number): number {
  return Math.max(min, Math.min(max, value));
}
