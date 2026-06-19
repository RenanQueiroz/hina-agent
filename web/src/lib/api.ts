// Typed API client. Shapes come from api.gen.ts (generated from internal/wire).
import type {
  AdminLLMInfo,
  AdminUser,
  ConfigInfo,
  Conversation,
  LoginResponse,
  PostMessageResponse,
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
};
