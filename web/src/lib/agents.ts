// Pure helpers for the callable-agent UI (Phase 8). The login-frame reducer folds
// the streamed AgentLoginFrame events into a renderable state (output lines +
// detected URLs/codes + whether a paste is expected + the terminal outcome). Keeping
// it pure makes the streaming logic unit-testable without a browser/EventSource.
import type { AgentLoginFrame } from "./api.gen";

export interface LoginState {
  lines: string[];
  urls: string[];
  codes: string[];
  needsInput: boolean;
  done?: { ok: boolean; error: string };
}

export const emptyLoginState: LoginState = {
  lines: [],
  urls: [],
  codes: [],
  needsInput: false,
};

// reduceLoginFrame applies one streamed frame to the login state. URLs/codes are
// de-duplicated so a code that arrives on several partial lines is shown once.
export function reduceLoginFrame(
  state: LoginState,
  f: AgentLoginFrame,
): LoginState {
  switch (f.type) {
    case "output":
      return f.text ? { ...state, lines: [...state.lines, f.text] } : state;
    case "hint": {
      if (!f.hint) return state;
      if (f.hint.kind === "url" && !state.urls.includes(f.hint.value)) {
        return { ...state, urls: [...state.urls, f.hint.value] };
      }
      if (f.hint.kind === "code" && !state.codes.includes(f.hint.value)) {
        return { ...state, codes: [...state.codes, f.hint.value] };
      }
      if (f.hint.kind === "prompt") return { ...state, needsInput: true };
      return state;
    }
    case "done":
      return {
        ...state,
        needsInput: false,
        done: { ok: !!f.ok, error: f.error ?? "" },
      };
    default:
      return state;
  }
}

// authTypeLabel renders a profile auth type as a short human label.
export function authTypeLabel(authType: string): string {
  switch (authType) {
    case "browser_state":
      return "Browser login";
    case "api_key":
      return "API key";
    case "oauth_token":
      return "OAuth token";
    case "local_llamacpp":
      return "Local (llama.cpp)";
    default:
      return authType;
  }
}
