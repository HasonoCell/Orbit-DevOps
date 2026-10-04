import { useCallback, useEffect, useRef, useState } from "react";

/** 查询成功、失效和重渲染都不续期；只有资源切换和明确的用户动作重启预算。 */
export function useObservationWindow(resource: string, duration = 5 * 60_000) {
  const [until, setUntil] = useState(() => Date.now() + duration);
  const [paused, setPaused] = useState(false);
  const previous = useRef(resource);
  const restart = useCallback(() => {
    setUntil(Date.now() + duration);
    setPaused(false);
  }, [duration]);
  useEffect(() => {
    if (previous.current !== resource) {
      previous.current = resource;
      restart();
    }
  }, [resource, restart]);
  useEffect(() => {
    const timer = window.setTimeout(
      () => setPaused(true),
      Math.max(0, until - Date.now()),
    );
    return () => window.clearTimeout(timer);
  }, [until]);
  return { until, paused, restart };
}
