// Typed API client. Shapes come from api.gen.ts (generated from internal/wire).
import type {
  AdminLLMInfo,
  AdminRuntime,
  AdminSandbox,
  AdminUser,
  ConfigInfo,
  Conversation,
  LoginResponse,
  PostMessageResponse,
  RTCStats,
  SandboxEnvironment,
  Secret,
  Turn,
  User,
} from "./api.gen";

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

async function req<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    credentials: "include",
    headers: { "content-type": "application/json", ...(init?.headers ?? {}) },
    ...init,
  });
  if (!res.ok) {
    let msg = res.statusText;
    try {
      const body = await res.json();
      if (body?.error) msg = body.error;
    } catch {
      /* non-JSON error body */
    }
    throw new ApiError(res.status, msg);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export const api = {
  getMe: () => req<LoginResponse>("/api/v1/auth/me").then((r) => r.user),
  login: (username: string, password: string): Promise<User> =>
    req<LoginResponse>("/api/v1/auth/login", {
      method: "POST",
      body: JSON.stringify({ username, password }),
    }).then((r) => r.user),
  logout: () => req<void>("/api/v1/auth/logout", { method: "POST" }),
  changePassword: (current_password: string, new_password: string) =>
    req<{ ok: boolean }>("/api/v1/auth/change-password", {
      method: "POST",
      body: JSON.stringify({ current_password, new_password }),
    }),

  getConfig: () => req<ConfigInfo>("/api/v1/config"),

  listConversations: (): Promise<Conversation[]> =>
    req<{ conversations: Conversation[] }>("/api/v1/conversations").then(
      (r) => r.conversations ?? [],
    ),
  createConversation: (title: string) =>
    req<Conversation>("/api/v1/conversations", {
      method: "POST",
      body: JSON.stringify({ title }),
    }),
  getTurns: (id: string): Promise<Turn[]> =>
    req<{ turns: Turn[] }>(`/api/v1/conversations/${id}/turns`).then(
      (r) => r.turns ?? [],
    ),
  postMessage: (id: string, text: string, signal?: AbortSignal) =>
    req<PostMessageResponse>(`/api/v1/conversations/${id}/messages`, {
      method: "POST",
      body: JSON.stringify({ text }),
      signal,
    }),

  adminUsers: (): Promise<AdminUser[]> =>
    req<{ users: AdminUser[] }>("/api/v1/admin/users").then(
      (r) => r.users ?? [],
    ),
  adminLLM: () => req<AdminLLMInfo>("/api/v1/admin/llm"),
  adminRTC: () => req<RTCStats>("/api/v1/admin/rtc"),
  adminRuntime: () => req<AdminRuntime>("/api/v1/admin/runtime"),
  adminSandbox: () => req<AdminSandbox>("/api/v1/admin/sandbox"),

  // Sandbox Environment policy (per user).
  getSandboxEnvironment: () =>
    req<SandboxEnvironment>("/api/v1/sandbox/environment"),
  updateSandboxEnvironment: (env: SandboxEnvironment) =>
    req<SandboxEnvironment>("/api/v1/sandbox/environment", {
      method: "PUT",
      body: JSON.stringify(env),
    }),

  // Secret vault (per user). Values are write-only — the API never returns them.
  listSecrets: (): Promise<Secret[]> =>
    req<{ secrets: Secret[] }>("/api/v1/sandbox/secrets").then(
      (r) => r.secrets ?? [],
    ),
  createSecret: (name: string, value: string, description = "") =>
    req<Secret>("/api/v1/sandbox/secrets", {
      method: "POST",
      body: JSON.stringify({ name, value, description }),
    }),
  deleteSecret: (id: string) =>
    req<void>(`/api/v1/sandbox/secrets/${id}`, { method: "DELETE" }),

  // Approve or deny a pending tool call raised in a conversation.
  decideToolApproval: (conversationId: string, callId: string, approve: boolean) =>
    req<{ ok: boolean }>(
      `/api/v1/conversations/${conversationId}/tool-approvals/${callId}`,
      { method: "POST", body: JSON.stringify({ approve }) },
    ),

  // Speak text into the caller's active live session (server-driven TTS).
  speak: (text: string, voice?: string, lang?: string) =>
    req<{ status: string }>("/api/v1/realtime/speak", {
      method: "POST",
      body: JSON.stringify({ text, voice, lang }),
    }),
};
