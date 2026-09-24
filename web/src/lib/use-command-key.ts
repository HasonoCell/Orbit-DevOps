import { useRef } from "react";

// 同一份表单数据的重试复用幂等键；编辑输入才视为新的逻辑命令。
export function useCommandKey() {
  const current = useRef<{ fingerprint: string; key: string } | null>(null);
  return {
    forPayload(payload: unknown): string {
      const fingerprint = JSON.stringify(payload);
      if (current.current?.fingerprint !== fingerprint) {
        current.current = { fingerprint, key: crypto.randomUUID() };
      }
      return current.current.key;
    },
    clear(): void { current.current = null; },
  };
}
