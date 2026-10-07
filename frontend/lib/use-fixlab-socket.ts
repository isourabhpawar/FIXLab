"use client";

import { useEffect, useRef } from "react";
import { wsUrl } from "@/lib/api";
import { useSessionStore } from "@/store/use-session-store";
import type { WsEvent } from "@/types/fixlab";

// useFixLabSocket maintains the live WebSocket event stream for a
// session (spec §19), with exponential-backoff reconnect. The stream
// stays open across FIX reconnects; it only closes when the session is
// destroyed/expired (the server closes the channel) or the component
// unmounts.
export function useFixLabSocket(token: string | null) {
  const addEvent = useSessionStore((s) => s.addEvent);
  const setWsConnected = useSessionStore((s) => s.setWsConnected);
  const refs = useRef({ addEvent, setWsConnected });
  refs.current = { addEvent, setWsConnected };

  useEffect(() => {
    if (!token) return;
    let ws: WebSocket | null = null;
    let cancelled = false;
    let retry = 0;
    let timer: ReturnType<typeof setTimeout> | null = null;

    const connect = () => {
      if (cancelled) return;
      const socket = new WebSocket(wsUrl(token));
      ws = socket;

      socket.onopen = () => {
        retry = 0;
        refs.current.setWsConnected(true);
      };

      socket.onmessage = (msg) => {
        try {
          const ev = JSON.parse(msg.data as string) as WsEvent;
          refs.current.addEvent(ev.type, ev.payload, ev.timestamp);
        } catch {
          /* malformed event: ignore */
        }
      };

      const scheduleReconnect = () => {
        refs.current.setWsConnected(false);
        if (cancelled) return;
        retry += 1;
        const delay = Math.min(1000 * 2 ** retry, 10000);
        timer = setTimeout(connect, delay);
      };

      socket.onclose = (e) => {
        // Normal closure from the server means the session is gone —
        // do not reconnect into a dead session.
        if (e.code === 1000) {
          refs.current.setWsConnected(false);
          return;
        }
        scheduleReconnect();
      };
      socket.onerror = () => socket.close();
    };

    connect();
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
      ws?.close();
    };
  }, [token]);
}
